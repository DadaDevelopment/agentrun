# agentrun

Run a production Dada agent on your laptop (or a CI box) straight from its agent
repo such as [tg-agent-tools](https://github.com/DadaDevelopment/tg-agent-tools).
Same image as prod, your prompt, the prod MCP tools. The repo is the spec:
`agents/<name>/core.md` is the prompt. Edit, `up`, eval.

```
eval.py / persona_eval.py --url http://127.0.0.1:18081/   -> agent (A2A), like prod eval

agent    ghcr.io/dadadevelopment/dada-cloud-kagent-app (public), config.json rendered from the repo
tools    console (DADA_TOKEN) / agentrun.toml [[tools]] / --mcp URL, in that priority;
         in-cluster .svc.cluster.local tool URLs are rewritten to the app's public URL
```

## Install

```bash
uv tool install git+https://github.com/DadaDevelopment/agentrun
```

or `pipx install git+https://github.com/DadaDevelopment/agentrun`. Stdlib only,
Python >= 3.10. Prerequisites: Docker (compose v2) and `docker login ghcr.io`
(a GitHub PAT with `read:packages`). On Apple Silicon the image runs under amd64
emulation, first turn ~15 s.

## Agent repo setup

`agentrun.toml` next to `agents/`:

```toml
project = "agent-sandbox"
env = "prod"
agent = "tg-vibecoder"
prompt = "agents/tg-vibecoder/core.md"
# optional:
# model = "glm-5.3-flash"
# model_base_url = "https://api.z.ai/api/coding/paas/v4"
# image = "ghcr.io/dadadevelopment/dada-cloud-kagent-app:f74abba2"
# description = "..."
# max_tokens = 2048
# reasoning_effort = "low"

# optional static tools (only needed if DADA_TOKEN is not set):
# [[tools]]
# url = "https://tg-agent-tools-624109.dada-tuda.ru/mcp"
# [tools.headers]
# Authorization = "Bearer ..."
```

`.agentrun.env` (gitignored) or the environment:

```bash
MODEL_API_KEY=...            # required
MODEL=glm-5.3-flash          # optional
MODEL_BASE_URL=...           # optional
DADA_TOKEN=...               # console token, pulls the agent's tools
# or DADA_CLIENT_ID / DADA_CLIENT_SECRET for the client-credentials flow
```

## Run

```bash
cd tg-agent-tools
echo '.agentrun/' >> .gitignore          # rendered config + env, mode 0600
agentrun up                              # render + start, first turn needs the image pull
agentrun smoke                           # one real A2A turn, exit 1 on silence
python3 eval.py --url http://127.0.0.1:18081/
agentrun logs -f
agentrun down
```

Prompt edits need `agentrun up` again (config.json is rendered at `up`).
`agentrun up --core agents/<name>/experiments/foo.md` renders a different prompt
without touching the repo spec; `up --mcp http://.../mcp` overrides the tool
list.

## State

`.agentrun/` holds the rendered `config.json`, `agent-card.json`, env files
(mode 0600) and the compose `.env`. Delete it to start clean.
