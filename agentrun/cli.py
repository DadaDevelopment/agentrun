#!/usr/bin/env python3
"""agentrun: run the production agent chain on a laptop or CI box from an agent repo.

The agent repo already is the spec: `agents/<name>/core.md` is the prompt the
kagent Agent runs, `agents/<name>/domains/*.md` are the procedures agent-runtime
serves through load_skill. Nothing is rendered or copied: core.md is injected
into the prod agent config verbatim and the repo is mounted as GITOPS_BASE_PATH,
so the runtime reads domains from the working tree. Edit, re-run, eval.

Chain (same images as prod, same env, same MCP dependencies):

    eval.py / persona_eval.py --url http://127.0.0.1:18081/   (A2A, agent directly)
    qa/harness.py QA_RUNTIME_URL=http://127.0.0.1:18083        (/message, full runtime)

    runtime  ghcr.io/dadadevelopment/dada-cloud-console-agent-runtime:<prod tag>
             + postgres 16 with the console migrations of the same tag
    agent    ghcr.io/dadadevelopment/dada-cloud-kagent-app:<prod tag>, prod config.json
             with instruction=core.md and http_tools url=--mcp
    mcp      owner's choice: --mcp local builds the repo Dockerfile with the prod
             env (prod Postgres, prod CRM, throwaway redis); --mcp <url> uses any
             server the owner runs. The prod in-cluster hostnames of runtime and
             MCP are network aliases here, so the manifests stored in the prod
             tool_manifests table resolve to the local containers unchanged.

Everything that describes prod is pulled with kubectl into <repo>/.agentrun/prod
(read-only kubectl get; nothing is written to the cluster) and cached, so a box
without cluster access can still run from the last snapshot. Secrets land in
<repo>/.agentrun with mode 0600; add `.agentrun/` to the repo's .gitignore.
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import shutil
import signal
import socket
import stat
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

TOOLS_DIR = Path(__file__).resolve().parent
COMPOSE_FILE = TOOLS_DIR / "compose.yaml"

DROP_AGENT_ENV_PREFIXES = ("OTEL_", "LANGFUSE_")
DROP_AGENT_ENV = {"PROMPT_VERSION", "KAGENT_URL", "KAGENT_NAME", "KAGENT_NAMESPACE"}
DROP_MCP_ENV = {"PYTHONPATH", "UVICORN_LOG_CONFIG"}
SECRET_PLACEHOLDER = "__SECRET__"


def die(message: str, code: int = 1) -> None:
    print(f"agentrun: {message}", file=sys.stderr)
    sys.exit(code)


def run(cmd: list[str], **kwargs) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, check=True, text=True, **kwargs)


def kubectl_json(namespace: str, kind: str, name: str) -> dict:
    out = run(["kubectl", "--request-timeout=20s", "-n", namespace, "get", kind, name, "-o", "json"],
              capture_output=True).stdout
    return json.loads(out)


def secret_value(namespace: str, name: str, key: str) -> str:
    data = kubectl_json(namespace, "secret", name)["data"]
    if key not in data:
        die(f"secret {namespace}/{name} has no key {key}")
    return base64.b64decode(data[key]).decode()


def write_private(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content)
    path.chmod(stat.S_IRUSR | stat.S_IWUSR)


def env_file(pairs: dict) -> str:
    lines = []
    for key, value in pairs.items():
        if value is None:
            continue
        text = str(value)
        if any(ch in text for ch in " #\"'$\n"):
            text = json.dumps(text)
        lines.append(f"{key}={text}")
    return "\n".join(lines) + "\n"


def container_env(container: dict, resolve_secret) -> dict:
    """Flatten a pod container env list. Secret refs are resolved through resolve_secret;
    fieldRef/configMapKeyRef entries are returned as None so callers set them explicitly."""
    result = {}
    for entry in container.get("env", []):
        name = entry["name"]
        if "value" in entry:
            result[name] = entry["value"]
            continue
        source = entry.get("valueFrom", {})
        if "secretKeyRef" in source:
            ref = source["secretKeyRef"]
            result[name] = resolve_secret(ref["name"], ref["key"])
        else:
            result[name] = None
    return result


def db_forward_target(pod_spec: dict, database_url: str) -> dict | None:
    """The prod MCP reaches Postgres through a hostAlias that points at an in-cluster
    Service (pg-router); that hostname resolves to a different server from outside.
    Find the Service behind the alias so `up` can kubectl port-forward to it."""
    host = urlsplit(database_url).hostname
    if not host:
        return None
    ip = None
    for alias in pod_spec.get("hostAliases", []):
        if host in alias.get("hostnames", []):
            ip = alias["ip"]
    if ip is None:
        return None
    services = json.loads(run(["kubectl", "--request-timeout=30s", "get", "svc", "-A", "-o", "json"],
                              capture_output=True).stdout)["items"]
    for svc in services:
        if svc["spec"].get("clusterIP") == ip:
            return {"namespace": svc["metadata"]["namespace"], "service": svc["metadata"]["name"],
                    "port": urlsplit(database_url).port or 5432, "host": host}
    die(f"hostAlias {host} -> {ip} matches no Service clusterIP")


def port_open(port: int) -> bool:
    with socket.socket() as sock:
        sock.settimeout(0.5)
        return sock.connect_ex(("127.0.0.1", port)) == 0


def pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except OSError:
        return False
    return True


def start_db_forward(paths: Paths, target: dict, local_port: int) -> None:
    pid_file = paths.state / "pgforward.pid"
    if pid_file.exists() and pid_alive(int(pid_file.read_text())) and port_open(local_port):
        return
    if port_open(local_port):
        die(f"port {local_port} busy and not ours; pass --db-port")
    log = open(paths.state / "pgforward.log", "ab")
    proc = subprocess.Popen(["kubectl", "-n", target["namespace"], "port-forward", "--address", "127.0.0.1",
                             f"svc/{target['service']}", f"{local_port}:{target['port']}"],
                            stdout=log, stderr=log, start_new_session=True)
    pid_file.write_text(str(proc.pid))
    for _ in range(100):
        if port_open(local_port):
            print(f"agentrun: port-forward {target['namespace']}/svc/{target['service']} -> 127.0.0.1:{local_port} (pid {proc.pid})", flush=True)
            return
        if proc.poll() is not None:
            die(f"kubectl port-forward exited {proc.returncode}; see {paths.state / 'pgforward.log'}")
        time.sleep(0.2)
    die("port-forward did not open in 20s")


def stop_db_forward(paths: Paths) -> None:
    pid_file = paths.state / "pgforward.pid"
    if not pid_file.exists():
        return
    pid = int(pid_file.read_text())
    if pid_alive(pid):
        os.killpg(pid, signal.SIGTERM)
    pid_file.unlink()


def swap_base(url: str, new_base: str) -> str:
    """Keep the path of `url`, take scheme+host from `new_base`."""
    old = urlsplit(url)
    new = urlsplit(new_base)
    return urlunsplit((new.scheme, new.netloc, old.path, old.query, old.fragment))


class Paths:
    def __init__(self, repo: Path, agent: str):
        self.repo = repo
        self.agent = agent
        self.spec = repo / "agents" / agent
        self.state = repo / ".agentrun"
        self.prod = self.state / "prod"
        self.agent_cfg = self.state / "agent"
        self.migrations = self.state / "migrations"
        self.secrets = self.state / "secrets.json"
        self.compose_env = self.state / "compose.env"


def detect_agent(repo: Path, explicit: str | None) -> str:
    if explicit:
        return explicit
    agents_dir = repo / "agents"
    candidates = sorted(p.name for p in agents_dir.iterdir() if (p / "core.md").exists()) if agents_dir.is_dir() else []
    if len(candidates) != 1:
        die(f"pass --agent; found agents with core.md under {agents_dir}: {candidates}")
    return candidates[0]


def pull(paths: Paths, args: argparse.Namespace) -> None:
    """Snapshot everything prod-specific with read-only kubectl gets."""
    print(f"agentrun: pulling prod snapshot into {paths.prod}", flush=True)
    paths.prod.mkdir(parents=True, exist_ok=True)

    agent_secret = kubectl_json(args.agent_ns, "secret", paths.agent)["data"]
    for key in ("config.json", "agent-card.json"):
        (paths.prod / key).write_bytes(base64.b64decode(agent_secret[key]))

    agent_deploy = kubectl_json(args.agent_ns, "deployment", paths.agent)
    agent_container = agent_deploy["spec"]["template"]["spec"]["containers"][0]
    runtime_deploy = kubectl_json(args.runtime_ns, "deployment", args.runtime_deploy)
    runtime_container = runtime_deploy["spec"]["template"]["spec"]["containers"][0]

    secrets = {}

    def resolver(scope: str, namespace: str):
        def resolve(name, key):
            secrets[f"{scope}:{key}"] = secret_value(namespace, name, key)
            return SECRET_PLACEHOLDER
        return resolve

    agent_env = container_env(agent_container, resolver("agent", args.agent_ns))
    runtime_env = container_env(runtime_container, resolver("runtime", args.runtime_ns))
    for key in ("AGENT_RUNTIME_TOKEN", "AGENT_PAUSE_CRM_TOKEN"):
        secrets[f"runtime:{key}"] = secret_value(args.runtime_ns, args.runtime_secret, key)

    snapshot = {
        "pulled_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "agent_image": agent_container["image"],
        "agent_env": {k: v for k, v in agent_env.items() if v is not None and not k.startswith(DROP_AGENT_ENV_PREFIXES)},
        "runtime_image": runtime_container["image"],
        "runtime_env": {k: v for k, v in runtime_env.items() if v is not None},
    }

    if args.mcp == "local":
        mcp_deploy = kubectl_json(args.mcp_ns, "deployment", args.mcp_deploy)
        mcp_pod = mcp_deploy["spec"]["template"]["spec"]
        mcp_container = mcp_pod["containers"][0]
        mcp_env = container_env(mcp_container, resolver("mcp", args.mcp_ns))
        snapshot["mcp_image"] = mcp_container["image"]
        snapshot["mcp_env"] = {k: v for k, v in mcp_env.items() if v is not None}
        snapshot["mcp_db_forward"] = db_forward_target(mcp_pod, secrets.get("mcp:DATABASE_URL") or mcp_env.get("DATABASE_URL", ""))

    (paths.prod / "snapshot.json").write_text(json.dumps(snapshot, ensure_ascii=False, indent=2))
    write_private(paths.secrets, json.dumps(secrets, ensure_ascii=False, indent=2))
    print(f"agentrun: snapshot ok: agent {snapshot['agent_image']}, runtime {snapshot['runtime_image']}", flush=True)


def ensure_migrations(paths: Paths, runtime_image: str) -> None:
    tag = runtime_image.rsplit(":", 1)[-1]
    marker = paths.migrations / ".tag"
    if marker.exists() and marker.read_text().strip() == tag:
        return
    backend_image = f"ghcr.io/dadadevelopment/dada-cloud-console-backend:{tag}"
    print(f"agentrun: extracting migrations from {backend_image}", flush=True)
    if paths.migrations.exists():
        shutil.rmtree(paths.migrations)
    paths.migrations.mkdir(parents=True)
    cid = run(["docker", "create", "--platform", "linux/amd64", backend_image], capture_output=True).stdout.strip()
    try:
        tmp = paths.state / "_migrations_tmp"
        if tmp.exists():
            shutil.rmtree(tmp)
        run(["docker", "cp", f"{cid}:/app/migrations", str(tmp)])
    finally:
        subprocess.run(["docker", "rm", "-f", cid], check=False, capture_output=True)
    count = 0
    for sql in sorted(tmp.glob("*.sql")):
        shutil.copy(sql, paths.migrations / sql.name)
        count += 1
    shutil.rmtree(tmp)
    marker.write_text(tag)
    print(f"agentrun: {count} migrations ready", flush=True)


def mcp_urls(args: argparse.Namespace) -> tuple[str, str]:
    """Return (url as containers see it, url as the host sees it)."""
    if args.mcp == "local":
        return f"http://mcp:8000/mcp", f"http://127.0.0.1:{args.mcp_port}/mcp"
    parts = urlsplit(args.mcp)
    if parts.scheme not in ("http", "https") or not parts.netloc:
        die(f"--mcp must be 'local' or an http(s) URL, got {args.mcp!r}")
    if parts.hostname in ("localhost", "127.0.0.1"):
        port = f":{parts.port}" if parts.port else ""
        inner = urlunsplit((parts.scheme, f"host.docker.internal{port}", parts.path, parts.query, parts.fragment))
        return inner, args.mcp
    return args.mcp, args.mcp


def render(paths: Paths, args: argparse.Namespace) -> dict:
    snapshot = json.loads((paths.prod / "snapshot.json").read_text())
    secrets = json.loads(paths.secrets.read_text())
    core_path = Path(args.core).resolve() if args.core else paths.spec / "core.md"
    if not core_path.exists():
        die(f"prompt not found: {core_path}")
    domains_dir = paths.spec / "domains"
    if not domains_dir.is_dir():
        die(f"domains dir not found: {domains_dir}")

    mcp_inner, mcp_host = mcp_urls(args)
    mcp_base = mcp_inner[: -len("/mcp")] if mcp_inner.endswith("/mcp") else mcp_inner
    agent_alias = f"{paths.agent}.kagent.svc.cluster.local"

    config = json.loads((paths.prod / "config.json").read_text())
    config["instruction"] = core_path.read_text()
    for tool in config.get("http_tools") or []:
        tool["params"]["url"] = mcp_inner
    card = json.loads((paths.prod / "agent-card.json").read_text())
    card["url"] = f"http://{agent_alias}:8080"
    for iface in card.get("supportedInterfaces") or []:
        iface["url"] = card["url"]
    paths.agent_cfg.mkdir(parents=True, exist_ok=True)
    (paths.agent_cfg / "config.json").write_text(json.dumps(config, ensure_ascii=False, indent=2))
    (paths.agent_cfg / "agent-card.json").write_text(json.dumps(card, ensure_ascii=False, indent=2))

    agent_env = {k: v for k, v in snapshot["agent_env"].items() if k not in DROP_AGENT_ENV}
    for key, value in secrets.items():
        if key.startswith("agent:"):
            agent_env[key.split(":")[-1]] = value
    agent_env.update({
        "KAGENT_NAME": paths.agent,
        "KAGENT_NAMESPACE": args.agent_ns,
        "KAGENT_URL": "http://kagent-controller.kagent:8083",
        "OTEL_TRACING_ENABLED": "false",
        "OTEL_LOGGING_ENABLED": "false",
    })
    write_private(paths.state / "agent.env", env_file(agent_env))

    runtime_env = dict(snapshot["runtime_env"])
    prod_mcp_host = None
    for key, value in list(runtime_env.items()):
        if key.startswith("AGENT_") and key.endswith("_CRM_URL") and value:
            prod_mcp_host = urlsplit(value).hostname
            runtime_env[key] = swap_base(value, mcp_base)
    runtime_env.pop("TG_GATEWAY_OUTBOUND_URL", None)
    runtime_env.update({
        "DB_URL": "postgres://dada:dada@db:5432/dada?sslmode=disable",
        "AUTH_MODE": "keycloak",
        "GITOPS_BASE_PATH": "/spec",
        "AGENT_RUNTIME_PORT": "8083",
        "AGENT_RUNTIME_IDLE_TICK_SECONDS": "0",
        "AGENT_RUNTIME_TOKEN": secrets["runtime:AGENT_RUNTIME_TOKEN"],
        "AGENT_PAUSE_CRM_TOKEN": secrets["runtime:AGENT_PAUSE_CRM_TOKEN"],
        "LOG_LEVEL": args.log_level,
    })
    write_private(paths.state / "runtime.env", env_file(runtime_env))

    if args.mcp == "local":
        mcp_env = {k: v for k, v in snapshot["mcp_env"].items() if k not in DROP_MCP_ENV}
        for key, value in secrets.items():
            if key.startswith("mcp:"):
                mcp_env[key.split(":", 1)[1]] = value
        mcp_env["REDIS_URL"] = "redis://mcp-redis:6379"
        mcp_env["PORT"] = "8000"
        forward = snapshot.get("mcp_db_forward")
        if forward:
            parts = urlsplit(mcp_env["DATABASE_URL"])
            netloc = f"{parts.username}:{parts.password}@host.docker.internal:{args.db_port}"
            mcp_env["DATABASE_URL"] = urlunsplit((parts.scheme, netloc, parts.path, parts.query, parts.fragment))
        write_private(paths.state / "mcp.env", env_file(mcp_env))
    elif not (paths.state / "mcp.env").exists():
        write_private(paths.state / "mcp.env", "")

    compose_env = {
        "AGENTRUN_PROJECT": f"agentrun-{paths.agent}",
        "AGENTRUN_STATE": str(paths.state),
        "AGENTRUN_TOOLS": str(TOOLS_DIR),
        "AGENTRUN_REPO": str(paths.repo),
        "AGENT_NAME": paths.agent,
        "AGENT_IMAGE": snapshot["agent_image"],
        "RUNTIME_IMAGE": snapshot["runtime_image"],
        "AGENT_PORT": str(args.agent_port),
        "RUNTIME_PORT": str(args.runtime_port),
        "MCP_PORT": str(args.mcp_port),
        "MCP_BUILD_CONTEXT": str(Path(args.mcp_build_context).resolve() if args.mcp_build_context else paths.repo),
        "MCP_PROD_HOST": prod_mcp_host or "mcp.local",
    }
    (paths.compose_env).write_text(env_file(compose_env))
    return {"mcp_host_url": mcp_host, "runtime_token": runtime_env["AGENT_RUNTIME_TOKEN"], "snapshot": snapshot}


def compose(paths: Paths, *argv: str, profile_mcp: bool = False, **kwargs) -> subprocess.CompletedProcess:
    cmd = ["docker", "compose", "--env-file", str(paths.compose_env), "-f", str(COMPOSE_FILE)]
    if profile_mcp:
        cmd += ["--profile", "mcp"]
    cmd += list(argv)
    return subprocess.run(cmd, text=True, **kwargs)


def cmd_up(args: argparse.Namespace) -> None:
    paths = Paths(Path(args.repo).resolve(), detect_agent(Path(args.repo).resolve(), args.agent))
    if not paths.spec.is_dir():
        die(f"no agent spec at {paths.spec}")
    if args.pull or not (paths.prod / "snapshot.json").exists():
        try:
            pull(paths, args)
        except subprocess.CalledProcessError as exc:
            if (paths.prod / "snapshot.json").exists() and not args.pull:
                print(f"agentrun: kubectl failed ({exc.stderr.strip()[:200]}), using cached snapshot", flush=True)
            else:
                die(f"kubectl failed and no cached snapshot: {exc.stderr.strip()[:400]}")
    rendered = render(paths, args)
    ensure_migrations(paths, rendered["snapshot"]["runtime_image"])
    local_mcp = args.mcp == "local"
    if local_mcp and rendered["snapshot"].get("mcp_db_forward"):
        start_db_forward(paths, rendered["snapshot"]["mcp_db_forward"], args.db_port)
    result = compose(paths, "up", "-d", "--wait", "--remove-orphans", profile_mcp=local_mcp)
    if result.returncode != 0:
        compose(paths, "ps", profile_mcp=local_mcp)
        die("compose up failed; see `agentrun logs`")
    print()
    print(f"agent (A2A, eval.py --url):   http://127.0.0.1:{args.agent_port}/")
    print(f"runtime (/message, harness):  http://127.0.0.1:{args.runtime_port}  Bearer = .agentrun/runtime.env AGENT_RUNTIME_TOKEN")
    print(f"mcp:                          {rendered['mcp_host_url']}")
    print(f"prompt: {Path(args.core).resolve() if args.core else paths.spec / 'core.md'}")
    print(f"domains: {paths.spec / 'domains'} (live mount, no restart needed)")
    print("prompt edits need `agentrun up` again (config.json is rendered at up); domain edits are live.")


def cmd_down(args: argparse.Namespace) -> None:
    paths = Paths(Path(args.repo).resolve(), detect_agent(Path(args.repo).resolve(), args.agent))
    if not paths.compose_env.exists():
        die("nothing to stop: no .agentrun/compose.env")
    argv = ["down", "--remove-orphans"]
    if args.volumes:
        argv.append("-v")
    compose(paths, *argv, profile_mcp=True)
    stop_db_forward(paths)


def cmd_logs(args: argparse.Namespace) -> None:
    paths = Paths(Path(args.repo).resolve(), detect_agent(Path(args.repo).resolve(), args.agent))
    argv = ["logs", "--tail", str(args.tail)]
    if args.follow:
        argv.append("-f")
    argv += args.service
    compose(paths, *argv, profile_mcp=True)


def cmd_status(args: argparse.Namespace) -> None:
    paths = Paths(Path(args.repo).resolve(), detect_agent(Path(args.repo).resolve(), args.agent))
    compose(paths, "ps", profile_mcp=True)


def post_json(url: str, body: dict, headers: dict, timeout: float) -> tuple[int, str]:
    req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST",
                                 headers={"Content-Type": "application/json", **headers})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode()


def cmd_smoke(args: argparse.Namespace) -> None:
    """One real turn through both entry points; exit 1 unless both produced text."""
    paths = Paths(Path(args.repo).resolve(), detect_agent(Path(args.repo).resolve(), args.agent))
    runtime_env = dict(line.split("=", 1) for line in (paths.state / "runtime.env").read_text().splitlines() if "=" in line)
    token = json.loads(runtime_env["AGENT_RUNTIME_TOKEN"]) if runtime_env["AGENT_RUNTIME_TOKEN"].startswith('"') else runtime_env["AGENT_RUNTIME_TOKEN"]
    ok = True

    chat = "-900" + str(uuid.uuid4().int)[:9]
    body = {
        "agent_name": paths.agent, "channel": "telegram", "external_id": chat,
        "actor": {"external_id": chat.lstrip("-"), "username": "agentrun_smoke", "first_name": "Smoke"},
        "messages": [{"content": args.text, "channel_message_id": uuid.uuid4().hex[:12]}],
    }
    started = time.monotonic()
    status, raw = post_json(f"http://127.0.0.1:{args.runtime_port}/message", body,
                            {"Authorization": f"Bearer {token}", "Accept": "application/x-ndjson"}, args.timeout)
    print(f"runtime /message -> HTTP {status} in {time.monotonic() - started:.1f}s")
    replies = []
    for line in raw.splitlines():
        if not line.strip():
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            print("  " + line[:300])
            continue
        print("  " + json.dumps(event, ensure_ascii=False)[:400])
        result = event.get("result") if isinstance(event.get("result"), dict) else {}
        if isinstance(result.get("text"), str) and result["text"].strip():
            replies.append(result["text"])
    if status != 200 or not replies:
        ok = False

    message = {"role": "user", "messageId": uuid.uuid4().hex, "parts": [{"kind": "text", "text": args.text}]}
    started = time.monotonic()
    status, raw = post_json(f"http://127.0.0.1:{args.agent_port}/",
                            {"jsonrpc": "2.0", "id": uuid.uuid4().hex, "method": "message/send", "params": {"message": message}},
                            {}, args.timeout)
    print(f"agent A2A message/send -> HTTP {status} in {time.monotonic() - started:.1f}s")
    text = ""
    try:
        data = json.loads(raw)
        if data.get("error"):
            print("  error: " + json.dumps(data["error"], ensure_ascii=False)[:400])
        result = data.get("result") or {}
        text = "\n".join(p.get("text", "") for a in result.get("artifacts", []) for p in a.get("parts", []))
        print("  " + (text[:600] or json.dumps(result, ensure_ascii=False)[:400]))
    except json.JSONDecodeError:
        print("  " + raw[:400])
    if status != 200 or not text.strip():
        ok = False
    print("smoke:", "OK" if ok else "FAIL")
    sys.exit(0 if ok else 1)


def main() -> None:
    parser = argparse.ArgumentParser(prog="agentrun", description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="cmd", required=True)

    def common(p):
        p.add_argument("--repo", default=".", help="agent repo root (holds agents/<name>/core.md)")
        p.add_argument("--agent", help="agent name; auto when exactly one agents/*/core.md exists")

    def prod_refs(p):
        p.add_argument("--agent-ns", default="kagent")
        p.add_argument("--runtime-ns", default="argocd-prod")
        p.add_argument("--runtime-deploy", default="dada-cloud-console-agent-runtime")
        p.add_argument("--runtime-secret", default="tg-referral-runtime")
        p.add_argument("--mcp-ns", default="agent-sandbox-prod")
        p.add_argument("--mcp-deploy", default="tg-agent-tools-deploy")

    def ports(p):
        p.add_argument("--agent-port", type=int, default=18081)
        p.add_argument("--runtime-port", type=int, default=18083)
        p.add_argument("--mcp-port", type=int, default=18000)

    up = sub.add_parser("up", help="pull prod snapshot (first time or --pull), render, start the chain")
    common(up)
    prod_refs(up)
    ports(up)
    up.add_argument("--mcp", required=True, help="'local' (build repo Dockerfile with prod env) or an MCP URL the owner runs")
    up.add_argument("--mcp-build-context", help="Dockerfile context for --mcp local (default: --repo)")
    up.add_argument("--core", help="prompt file instead of agents/<name>/core.md (e.g. experiments/*.md)")
    up.add_argument("--pull", action="store_true", help="refresh the prod snapshot even if cached")
    up.add_argument("--db-port", type=int, default=18432, help="local port of the kubectl port-forward to the prod MCP Postgres (--mcp local)")
    up.add_argument("--log-level", default="info")
    up.set_defaults(func=cmd_up)

    down = sub.add_parser("down", help="stop the chain")
    common(down)
    down.add_argument("-v", "--volumes", action="store_true", help="also drop the local postgres data")
    down.set_defaults(func=cmd_down)

    logs = sub.add_parser("logs")
    common(logs)
    logs.add_argument("service", nargs="*")
    logs.add_argument("-f", "--follow", action="store_true")
    logs.add_argument("--tail", default=200)
    logs.set_defaults(func=cmd_logs)

    status = sub.add_parser("status")
    common(status)
    status.set_defaults(func=cmd_status)

    smoke = sub.add_parser("smoke", help="one real turn through runtime and agent; exit 1 on empty reply")
    common(smoke)
    ports(smoke)
    smoke.add_argument("--text", default="Привет, кто вы?")
    smoke.add_argument("--timeout", type=float, default=240)
    smoke.set_defaults(func=cmd_smoke)

    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
