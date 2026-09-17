"""agentrun: run a Dada agent from its repo on a laptop or CI box.

The repo is the spec: ``agentrun.toml`` names project/env/agent and the prompt
file (default ``agents/<name>/core.md``). Tool URLs and headers come from the
console (``DADA_TOKEN``) or from ``[[tools]]`` in agentrun.toml. Model
credentials come from ``MODEL_API_KEY`` / ``MODEL`` / ``MODEL_BASE_URL``
in the environment or ``.agentrun.env`` next to agentrun.toml. No kubectl, no
docker login: only Docker and the public agent image.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
import tomllib
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


def load_spec(repo: Path) -> dict:
    path = repo / "agentrun.toml"
    if not path.exists():
        die(f"{path} not found; see README")
    spec = tomllib.loads(path.read_text())
    for key in ("project", "env", "agent", "prompt"):
        if not spec.get(key):
            die(f"agentrun.toml: '{key}' is required")
    load_dotenv(repo / ".agentrun.env")
    return spec


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
    ref = console_get(token, "/api/v1/resolve?" + urllib.parse.urlencode({"project": spec["project"], "env": spec["env"]}))
    pid, eid = ref["project"]["id"], ref["environment"]["id"]
    agents = console_get(token, f"/api/v1/projects/{pid}/environments/{eid}/agents")
    items = agents if isinstance(agents, list) else agents.get("items") or agents.get("agents") or []
    for a in items:
        if a.get("name") != spec["agent"]:
            continue
        tools = ((a.get("summary_json") or {}).get("spec") or {}).get("tools") or []
        out = []
        for t in tools:
            url = t.get("url") or ""
            host = urllib.parse.urlsplit(url).hostname or ""
            if host.endswith(".svc.cluster.local"):
                app = host.split(".")[0].removesuffix("-service")
                public = resolve_app(token, spec["project"], spec["env"], app).get("app", {}).get("url")
                if not public:
                    die(f"tool {t.get('name')}: app {app} has no public url; set [[tools]] url in agentrun.toml")
                url = public.rstrip("/") + urllib.parse.urlsplit(url).path
            out.append({"url": url, "headers": {h["name"]: h["value"] for h in t.get("headers") or []}, "timeout": t.get("timeout") or 30})
        return out
    die(f"agent {spec['agent']} not found in console {spec['project']}/{spec['env']}")
    return []


def tools_for(spec: dict, args: argparse.Namespace) -> list[dict]:
    if args.mcp:
        return [{"url": u, "headers": {}, "timeout": 30} for u in args.mcp]
    if spec.get("tools"):
        return [{"url": t["url"], "headers": dict(t.get("headers") or {}), "timeout": int(t.get("timeout") or 30)} for t in spec["tools"]]
    token = console_token()
    if not token:
        die("no tools: set [[tools]] in agentrun.toml, pass --mcp URL, or export DADA_TOKEN / DADA_CLIENT_ID+DADA_CLIENT_SECRET")
    tools = console_tools(token, spec)
    if not tools:
        die(f"console has no tools for {spec['agent']}; set [[tools]] in agentrun.toml or pass --mcp URL")
    return tools


def render(repo: Path, spec: dict, args: argparse.Namespace) -> Path:
    api_key = os.environ.get("MODEL_API_KEY")
    if not api_key:
        die("MODEL_API_KEY is not set (environment or .agentrun.env)")
    prompt_path = repo / (args.core or spec["prompt"])
    if not prompt_path.exists():
        die(f"prompt file {prompt_path} not found")
    tools = tools_for(spec, args)
    name = spec["agent"]
    config = {
        "model": {
            "type": "openai",
            "model": os.environ.get("MODEL", spec.get("model", DEFAULT_MODEL)),
            "base_url": os.environ.get("MODEL_BASE_URL", spec.get("model_base_url", DEFAULT_MODEL_BASE_URL)),
            "max_tokens": int(spec.get("max_tokens", 2048)),
            "reasoning_effort": spec.get("reasoning_effort", "low"),
            "api_format": "chatCompletions",
        },
        "description": spec.get("description", name),
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
        "AGENT_NAME": name, "AGENT_IMAGE": os.environ.get("AGENT_IMAGE", spec.get("image", DEFAULT_AGENT_IMAGE)),
        "AGENT_PORT": str(args.agent_port), "AGENTRUN_STATE": str(state), "AGENTRUN_TOOLS": str(TOOLS_DIR),
        "COMPOSE_PROJECT_NAME": f"agentrun-{name}",
    }
    (state / ".env").write_text("".join(f"{k}={v}\n" for k, v in dotenv.items()))
    print(f"agent {name}  prompt {prompt_path.relative_to(repo)}  model {config['model']['model']}")
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
    spec = load_spec(repo)
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


def main() -> None:
    p = argparse.ArgumentParser(prog="agentrun", description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--repo", default=".", help="agent repo containing agentrun.toml (default: cwd)")
    p.add_argument("--agent-port", type=int, default=18081)
    sub = p.add_subparsers(dest="cmd", required=True)

    up = sub.add_parser("up", help="render config from the repo and start the agent")
    up.add_argument("--core", help="prompt file to use instead of agentrun.toml 'prompt' (experiments)")
    up.add_argument("--mcp", action="append", metavar="URL", help="MCP server URL(s); overrides console/agentrun.toml tools")
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
    args.func(args)


if __name__ == "__main__":
    main()
