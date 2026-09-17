# agentrun

Run the production agent chain on your laptop (or a CI box) straight from an
agent repo such as [tg-agent-tools](https://github.com/DadaDevelopment/tg-agent-tools).
Same images as prod, same env, same MCP dependencies. The repo is the spec:
`agents/<name>/core.md` is the prompt, `agents/<name>/domains/*.md` are the
skills the runtime serves through `load_skill`. Edit, `up`, eval.

```
eval.py / persona_eval.py --url http://127.0.0.1:18081/   -> agent (A2A), like prod eval
qa/harness.py  QA_RUNTIME_URL=http://127.0.0.1:18083      -> agent-runtime (/message), full chain

runtime   ghcr.io/dadadevelopment/dada-cloud-console-agent-runtime:<prod tag> + postgres 16 with its migrations
agent     ghcr.io/dadadevelopment/dada-cloud-kagent-app:<prod tag>, prod config.json, instruction = core.md
mcp       --mcp local  builds the repo Dockerfile with prod env (prod Postgres via port-forward, prod CRM)
          --mcp <url>  any MCP server you run yourself
```

The prod in-cluster hostnames of runtime and MCP are docker network aliases, so
the tool manifests stored in the prod `tool_manifests` table resolve to the local
containers unchanged.

## Install

```bash
uv tool install git+https://github.com/DadaDevelopment/agentrun
```

or `pipx install git+https://github.com/DadaDevelopment/agentrun`. Stdlib only, Python >= 3.10.

## Prerequisites

| Need | Why |
|------|-----|
| Docker with compose v2 | runs the 5 containers. On Apple Silicon the two ghcr images are amd64 and run under emulation (first turn ~15 s). |
| `docker login ghcr.io` with a GitHub PAT that has `read:packages` in DadaDevelopment | images are private |
| `kubectl` pointed at the prod cluster with read access to ns `kagent`, `argocd-prod`, `agent-sandbox-prod`, `databases` | `up` pulls the agent Secret, runtime Deployment env, MCP Deployment env (read-only `kubectl get`, nothing is written), and `--mcp local` port-forwards the MCP Postgres |

Without cluster access `up` still works from the last cached snapshot in
`<repo>/.agentrun/` (but not `--mcp local`, it needs the DB forward).

## Run

```bash
cd tg-agent-tools
echo '.agentrun/' >> .gitignore          # snapshot + secrets live there, mode 0600
agentrun up --mcp local                  # ~2-3 min first time (image pull + MCP build)
agentrun smoke                           # one turn through /message and through A2A, exit 1 on silence
python3 eval.py --url http://127.0.0.1:18081/
agentrun logs -f mcp                     # or agent / runtime
agentrun down                            # add -v to drop the postgres volume too
```

Try a prompt experiment without touching `core.md`:

```bash
agentrun up --mcp local --core agents/tg-exchange-support/experiments/foo.md
```

`domains/*.md` are read from the working tree on every `load_skill`, no restart
needed. `core.md` (or `--core`) is baked into the agent config at `up`, so re-run
`up` after editing it.

Refresh the prod snapshot after a prod deploy: `agentrun up --mcp local --pull`.

## CI recipe

```bash
agentrun up --repo . --mcp local || exit 1
agentrun smoke --repo . || { agentrun logs --repo . ; agentrun down --repo . -v; exit 1; }
python3 eval.py --url http://127.0.0.1:18081/; rc=$?
agentrun down --repo . -v
exit $rc
```

Every subcommand exits non-zero on failure. `up` waits for all healthchecks.

## What is and is not prod

- Prod: both images and their tags, agent config.json/agent-card.json, runtime
  env, MCP env, MCP Postgres (through port-forward), Twenty CRM, GLM model key.
  Smoke and eval turns therefore create real CRM contacts under the eval chat ids.
- Local: postgres for agent-runtime conversation state (fresh per `up -v`),
  redis for MCP, in-memory A2A session store (`build(local=True)` instead of the
  kagent controller), OTEL/Langfuse disabled.
- Direct A2A (`eval.py`) has no `x-dada-*` headers, so `load_skill` returns 403
  on that leg exactly as against prod. The `/message` leg has the full chain.

## Ports

agent `18081`, runtime `18083`, mcp `18000`, DB forward `18432`. Override with
`--agent-port/--runtime-port/--mcp-port/--db-port`.
