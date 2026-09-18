"""agentrun: run a Dada agent from its own repo on a laptop or CI box.

The repo is the spec and ``.dada/agent.json`` is its only manifest, the same
one ``agentkit/repospec.py`` already gates. Everything that describes the agent
lives under ``agents/<name>/``:

    core.md                 system prompt, read by the runtime in production
    domains/<skill>.md      skills, served through load_skill
    runtime.yaml            model, tools, image: how to start this agent
    evals/suites/*.yaml     deterministic marker scenarios
    judge/*.yaml            the LLM-as-a-judge rules the same agent is scored by

There is no second manifest: no file outside ``.dada/agent.json`` knows the
agent name. Model credentials come from the environment or ``.agentrun.env``
(git-ignored) using the variable names ``runtime.yaml`` declares. No kubectl,
no docker login: only Docker and the public agent image.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from pathlib import Path

TOOLS_DIR = Path(__file__).resolve().parent
COMPOSE_FILE = TOOLS_DIR / "compose.yaml"
CONSOLE = "https://console.dada-tuda.ru"
TOKEN_URL = "https://id.dada-tuda.ru/realms/master/protocol/openid-connect/token"
DEFAULT_AGENT_IMAGE = "ghcr.io/kagent-dev/kagent/app:0.10.0-rc3"
DEFAULT_MODEL = "glm-5.3-flash"
DEFAULT_MODEL_BASE_URL = "https://api.z.ai/api/coding/paas/v4"


def die(message: str, code: int = 1) -> None:
    print(f"agentrun: {message}", file=sys.stderr)
    sys.exit(code)


def load_dotenv(path: Path) -> None:
    if not path.exists():
        return
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        os.environ.setdefault(key.strip(), value.strip().strip('"').strip("'"))


MANIFEST = Path(".dada") / "agent.json"


def load_manifest(repo: Path, agent: str | None) -> dict:
    """Return the manifest entry for one agent, with paths resolved.

    Reads ``.dada/agent.json`` (version 1 or 2). Version 2 adds the optional
    ``runtime`` / ``suites`` / ``judges`` / ``console`` / ``langfuse`` keys; a
    version 1 manifest still runs, it just has no runtime file to read.
    """
    path = repo / MANIFEST
    if not path.exists():
        die(f"{path} not found: the repo declares no agent (see docs/AGENT-REPO-SPEC.md)")
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        die(f"{MANIFEST}: {exc}")
    agents = data.get("agents")
    if not isinstance(agents, list) or not agents:
        die(f"{MANIFEST}: agents must be a non-empty list")

    if agent:
        entry = next((a for a in agents if a.get("name") == agent), None)
        if entry is None:
            die(f"{MANIFEST}: no agent named {agent!r}; have {[a.get('name') for a in agents]}")
    elif len(agents) == 1:
        entry = agents[0]
    else:
        die(f"pass --agent; {MANIFEST} declares {[a.get('name') for a in agents]}")

    name = entry.get("name")
    if not name:
        die(f"{MANIFEST}: an agent entry has no name")
    root = repo / entry.get("agents_root", ".")
    spec_dir = root / "agents" / name
    if not (spec_dir / "core.md").exists():
        die(f"{spec_dir / 'core.md'} not found: agents_root/name do not point at an agent")

    load_dotenv(repo / ".agentrun.env")
    return {
        "name": name,
        "dir": spec_dir,
        "prompt": spec_dir / "core.md",
        "runtime": repo / entry["runtime"] if entry.get("runtime") else spec_dir / "runtime.yaml",
        "suites": repo / entry["suites"] if entry.get("suites") else spec_dir / "evals" / "suites",
        "judges": repo / entry["judges"] if entry.get("judges") else spec_dir / "judge",
        "console": entry.get("console") or {},
        "langfuse": entry.get("langfuse") or {},
        "cases": repo / entry["cases"] if entry.get("cases") else None,
        "holdout_threshold": entry.get("holdout_threshold"),
    }


def load_runtime(path: Path) -> dict:
    """Parse runtime.yaml: model, tools, image. Missing file means defaults."""
    if not path.exists():
        return {}
    try:
        import yaml
    except ImportError:
        die("runtime.yaml needs PyYAML: pip install pyyaml")
    data = yaml.safe_load(path.read_text(encoding="utf-8")) or {}
    if not isinstance(data, dict):
        die(f"{path}: must hold a mapping")
    return data


def http_json(method: str, url: str, body=None, headers: dict | None = None, timeout: float = 30) -> tuple[int, dict | str]:
    data = None
    hdrs = dict(headers or {})
    if isinstance(body, dict):
        data = json.dumps(body).encode()
        hdrs.setdefault("Content-Type", "application/json")
    elif isinstance(body, bytes):
        data = body
    req = urllib.request.Request(url, data=data, method=method, headers=hdrs)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read().decode()
            status = resp.status
    except urllib.error.HTTPError as e:
        raw, status = e.read().decode(errors="replace"), e.code
    except (urllib.error.URLError, TimeoutError) as e:
        return 0, str(e)
    try:
        return status, json.loads(raw)
    except json.JSONDecodeError:
        return status, raw


def console_token() -> str | None:
    if os.environ.get("DADA_TOKEN"):
        return os.environ["DADA_TOKEN"]
    cid, secret = os.environ.get("DADA_CLIENT_ID"), os.environ.get("DADA_CLIENT_SECRET")
    if not (cid and secret):
        return None
    form = urllib.parse.urlencode({"grant_type": "client_credentials", "client_id": cid, "client_secret": secret}).encode()
    status, data = http_json("POST", TOKEN_URL, form, {"Content-Type": "application/x-www-form-urlencoded"})
    if status != 200 or not isinstance(data, dict):
        die(f"token request failed: HTTP {status} {str(data)[:200]}")
    return data["access_token"]


def console_get(token: str, path: str) -> dict | list:
    status, data = http_json("GET", CONSOLE + path, headers={"Authorization": f"Bearer {token}"})
    if status != 200:
        die(f"console GET {path} -> HTTP {status} {str(data)[:200]}")
    return data


def resolve_app(token: str, project: str, env: str, app: str) -> dict:
    q = urllib.parse.urlencode({"project": project, "env": env, "app": app})
    return console_get(token, f"/api/v1/resolve?{q}")


def console_tools(token: str, spec: dict) -> list[dict]:
    """Tools of the agent as the console knows them, in-cluster URLs rewritten to public ones."""
    console = spec["console"]
    project, env = console.get("project"), console.get("env")
    if not (project and env):
        die("console lookup needs console.project and console.env in .dada/agent.json")
    ref = console_get(token, "/api/v1/resolve?" + urllib.parse.urlencode({"project": project, "env": env}))
    pid, eid = ref["project"]["id"], ref["environment"]["id"]
    agents = console_get(token, f"/api/v1/projects/{pid}/environments/{eid}/agents")
    items = agents if isinstance(agents, list) else agents.get("items") or agents.get("agents") or []
    for a in items:
        if a.get("name") != spec["name"]:
            continue
        tools = ((a.get("summary_json") or {}).get("spec") or {}).get("tools") or []
        out = []
        for t in tools:
            url = t.get("url") or ""
            host = urllib.parse.urlsplit(url).hostname or ""
            if host.endswith(".svc.cluster.local"):
                app = host.split(".")[0].removesuffix("-service")
                public = resolve_app(token, project, env, app).get("app", {}).get("url")
                if not public:
                    die(f"tool {t.get('name')}: app {app} has no public url; set tools in runtime.yaml")
                url = public.rstrip("/") + urllib.parse.urlsplit(url).path
            out.append({"url": url, "headers": {h["name"]: h["value"] for h in t.get("headers") or []}, "timeout": t.get("timeout") or 30})
        return out
    die(f"agent {spec['name']} not found in console {project}/{env}")
    return []


def tool_headers(entry: dict) -> dict:
    """Headers for one runtime.yaml tool: literal headers plus any headers_env."""
    headers = dict(entry.get("headers") or {})
    env_name = entry.get("headers_env")
    if env_name:
        raw = os.environ.get(env_name)
        if not raw:
            die(f"runtime.yaml: tool declares headers_env {env_name} but it is not set")
        for part in raw.split("\n"):
            if ":" in part:
                key, value = part.split(":", 1)
                headers[key.strip()] = value.strip()
    return headers


def tools_for(spec: dict, runtime: dict, args: argparse.Namespace) -> list[dict]:
    if args.mcp:
        return [{"url": u, "headers": {}, "timeout": 30} for u in args.mcp]
    declared = runtime.get("tools") or []
    if declared:
        return [{"url": t["url"], "headers": tool_headers(t), "timeout": int(t.get("timeout") or 30)}
                for t in declared]
    token = console_token()
    if not token:
        die("no tools: declare them in runtime.yaml, pass --mcp URL, or export DADA_TOKEN / DADA_CLIENT_ID+DADA_CLIENT_SECRET")
    tools = console_tools(token, spec)
    if not tools:
        die(f"console has no tools for {spec['name']}; declare them in runtime.yaml or pass --mcp URL")
    return tools


def render(repo: Path, spec: dict, args: argparse.Namespace) -> Path:
    runtime = load_runtime(spec["runtime"])
    model = runtime.get("model") or {}
    key_env = model.get("api_key_env", "MODEL_API_KEY")
    api_key = os.environ.get(key_env)
    if not api_key:
        die(f"{key_env} is not set (environment or .agentrun.env)")
    prompt_path = Path(args.core).resolve() if args.core else spec["prompt"]
    if not prompt_path.exists():
        die(f"prompt file {prompt_path} not found")
    tools = tools_for(spec, runtime, args)
    name = spec["name"]
    config = {
        "model": {
            "type": "openai",
            "model": os.environ.get("MODEL", model.get("name", DEFAULT_MODEL)),
            "base_url": os.environ.get("MODEL_BASE_URL", model.get("base_url", DEFAULT_MODEL_BASE_URL)),
            "max_tokens": int(model.get("max_tokens", 2048)),
            "reasoning_effort": model.get("reasoning_effort", "low"),
            "api_format": model.get("api_format", "chatCompletions"),
        },
        "description": runtime.get("description", name),
        "instruction": prompt_path.read_text(),
        "http_tools": [
            {"params": {"url": t["url"], "headers": t["headers"], "timeout": t["timeout"], "terminate_on_close": True},
             "allowed_headers": ["x-dada-end-user", "x-dada-agent"]}
            for t in tools
        ],
        "stream": False,
    }
    a2a = "http://127.0.0.1:8080"
    card = {
        "name": name.replace("-", "_"), "description": config["description"], "version": "", "skills": [],
        "defaultInputModes": ["text"], "defaultOutputModes": ["text"], "capabilities": {"streaming": True},
        "url": a2a, "protocolVersion": "0.3", "preferredTransport": "JSONRPC",
        "supportedInterfaces": [{"url": a2a, "protocolBinding": "JSONRPC", "protocolVersion": v} for v in ("0.3", "1.0")],
    }
    state = repo / ".agentrun"
    (state / "agent").mkdir(parents=True, exist_ok=True)
    shutil.copy(TOOLS_DIR / "local_agent.py", state / "local_agent.py")
    (state / "agent" / "config.json").write_text(json.dumps(config, ensure_ascii=False, indent=1))
    (state / "agent" / "agent-card.json").write_text(json.dumps(card, ensure_ascii=False, indent=1))
    env = {
        "OPENAI_API_KEY": api_key, "KAGENT_NAME": name, "KAGENT_NAMESPACE": "kagent",
        "KAGENT_URL": "http://127.0.0.1:8083", "OTEL_TRACING_ENABLED": "false", "OTEL_LOGGING_ENABLED": "false",
    }
    env_path = state / "agent.env"
    env_path.write_text("".join(f"{k}={v}\n" for k, v in env.items()))
    env_path.chmod(0o600)
    dotenv = {
        "AGENT_NAME": name, "AGENT_IMAGE": os.environ.get("AGENT_IMAGE", runtime.get("image", DEFAULT_AGENT_IMAGE)),
        "AGENT_PORT": str(args.agent_port), "AGENTRUN_STATE": str(state), "AGENTRUN_TOOLS": str(TOOLS_DIR),
        "COMPOSE_PROJECT_NAME": f"agentrun-{name}",
    }
    (state / ".env").write_text("".join(f"{k}={v}\n" for k, v in dotenv.items()))
    shown = prompt_path.relative_to(repo) if prompt_path.is_relative_to(repo) else prompt_path
    print(f"agent {name}  prompt {shown}  model {config['model']['model']}")
    for t in tools:
        print(f"tool  {t['url']}" + ("  (+headers)" if t["headers"] else ""))
    return state


def compose(state: Path, *argv: str, **kwargs) -> subprocess.CompletedProcess:
    cmd = ["docker", "compose", "--env-file", str(state / ".env"), "-f", str(COMPOSE_FILE), *argv]
    return subprocess.run(cmd, **kwargs)


def state_dir(args: argparse.Namespace) -> Path:
    state = Path(args.repo).resolve() / ".agentrun"
    if not (state / ".env").exists():
        die("nothing rendered yet; run `agentrun up` first")
    return state


def cmd_up(args: argparse.Namespace) -> None:
    repo = Path(args.repo).resolve()
    spec = load_manifest(repo, args.agent)
    state = render(repo, spec, args)
    if compose(state, "up", "-d", "--wait", "--remove-orphans").returncode != 0:
        compose(state, "logs", "--tail", "80")
        die("agent did not become healthy")
    print(f"up: agent A2A http://127.0.0.1:{args.agent_port}/")


def cmd_down(args: argparse.Namespace) -> None:
    compose(state_dir(args), "down", "--remove-orphans")


def cmd_logs(args: argparse.Namespace) -> None:
    argv = ["logs", "--tail", str(args.tail)] + (["-f"] if args.follow else [])
    sys.exit(compose(state_dir(args), *argv).returncode)


def cmd_smoke(args: argparse.Namespace) -> None:
    """One real A2A turn; exit 1 unless the agent produced text."""
    message = {"role": "user", "messageId": uuid.uuid4().hex, "parts": [{"kind": "text", "text": args.text}]}
    started = time.monotonic()
    status, data = http_json("POST", f"http://127.0.0.1:{args.agent_port}/",
                             {"jsonrpc": "2.0", "id": uuid.uuid4().hex, "method": "message/send", "params": {"message": message}},
                             timeout=args.timeout)
    print(f"agent A2A message/send -> HTTP {status} in {time.monotonic() - started:.1f}s")
    text = ""
    if isinstance(data, dict):
        if data.get("error"):
            print("  error: " + json.dumps(data["error"], ensure_ascii=False)[:400])
        result = data.get("result") or {}
        text = "\n".join(p.get("text", "") for a in result.get("artifacts", []) for p in a.get("parts", []))
        print("  " + (text[:600] or json.dumps(result, ensure_ascii=False)[:400]))
    else:
        print("  " + str(data)[:400])
    ok = status == 200 and bool(text.strip())
    print("smoke:", "OK" if ok else "FAIL")
    sys.exit(0 if ok else 1)


def cmd_spec(args: argparse.Namespace) -> None:
    """Print what the manifest resolves to, so a broken layout is visible before `up`."""
    repo = Path(args.repo).resolve()
    spec = load_manifest(repo, args.agent)
    runtime = load_runtime(spec["runtime"])
    model = runtime.get("model") or {}
    suites = sorted(spec["suites"].glob("*.yaml")) if spec["suites"].is_dir() else []
    judges = sorted(spec["judges"].glob("*.yaml")) if spec["judges"].is_dir() else []
    domains = sorted((spec["dir"] / "domains").glob("*.md")) if (spec["dir"] / "domains").is_dir() else []

    print(f"agent      {spec['name']}")
    print(f"spec dir   {spec['dir']}")
    print(f"prompt     {spec['prompt'].name}  ({len(spec['prompt'].read_text())} chars)")
    print(f"domains    {len(domains)}: {', '.join(d.stem for d in domains) or '-'}")
    print(f"runtime    {spec['runtime'].name if spec['runtime'].exists() else 'MISSING'}"
          f"  model={model.get('name', '-')}  tools={len(runtime.get('tools') or [])}")
    print(f"suites     {len(suites)}: {', '.join(s.stem for s in suites) or '-'}")
    print(f"judges     {len(judges)}: {', '.join(j.stem for j in judges) or '-'}")
    if spec["cases"]:
        exists = "ok" if spec["cases"].exists() else "MISSING"
        print(f"cases      {spec['cases'].relative_to(repo)} ({exists}, threshold {spec['holdout_threshold']})")
    if spec["console"]:
        print(f"console    {spec['console'].get('project')}/{spec['console'].get('env')}")
    if spec["langfuse"]:
        print(f"langfuse   dataset prefix {spec['langfuse'].get('dataset_prefix')}")


def main() -> None:
    p = argparse.ArgumentParser(prog="agentrun", description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--repo", default=".", help="agent repo holding .dada/agent.json (default: cwd)")
    p.add_argument("--agent", help="agent name; required only when the manifest declares several")
    p.add_argument("--agent-port", type=int, default=18081)
    sub = p.add_subparsers(dest="cmd", required=True)

    spec = sub.add_parser("spec", help="show what the manifest resolves to")
    spec.set_defaults(func=cmd_spec)

    up = sub.add_parser("up", help="render config from the spec and start the agent")
    up.add_argument("--core", help="prompt file to use instead of agents/<name>/core.md (experiments)")
    up.add_argument("--mcp", action="append", metavar="URL", help="MCP server URL(s); overrides runtime.yaml tools")
    up.set_defaults(func=cmd_up)

    sub.add_parser("down", help="stop the agent").set_defaults(func=cmd_down)

    logs = sub.add_parser("logs")
    logs.add_argument("-f", "--follow", action="store_true")
    logs.add_argument("--tail", default="200")
    logs.set_defaults(func=cmd_logs)

    smoke = sub.add_parser("smoke", help="one A2A turn, exit 1 on silence")
    smoke.add_argument("--text", default="Привет")
    smoke.add_argument("--timeout", type=float, default=180)
    smoke.set_defaults(func=cmd_smoke)

    args = p.parse_args()
    try:
        args.func(args)
    except BrokenPipeError:
        os.dup2(os.open(os.devnull, os.O_WRONLY), sys.stdout.fileno())
        sys.exit(0)


if __name__ == "__main__":
    main()
