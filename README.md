# agentrun

Run a production Dada agent on your laptop (or a CI box) straight from its agent
repo such as [tg-agent-tools](https://github.com/DadaDevelopment/tg-agent-tools).
Same image as prod, your prompt, the prod MCP tools. The repo is the spec:
`agents/<name>/core.md` is the prompt. Edit, `up`, eval.

```
eval.py / persona_eval.py --url http://127.0.0.1:18081/   -> agent (A2A), like prod eval

agent    ghcr.io/kagent-dev/kagent/app (upstream, public), config.json rendered from the repo
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
# image = "ghcr.io/kagent-dev/kagent/app:0.10.0-rc3"
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

## CI

`.github/workflows/agent-eval.yml` runs the agent nightly (03:00 UTC) and on
`workflow_dispatch`, against the current `core.md` of the agent repo. One
source of truth: the cases live in `evals/*.yaml`, are synced into a Langfuse
dataset, and the experiment run in Langfuse produces the numbers that the CI
gate and the dashboard both read.

```
up + smoke     agent must answer at all, else the job fails early
sync dataset   scripts/langfuse_sync_dataset.py pushes evals/*.yaml into a
               Langfuse dataset (idempotent, items keyed by scenario id)
experiment     scripts/langfuse_experiment.py runs dataset.run_experiment:
               every turn is a traced A2A generation, every assertion is a
               Langfuse score (expect_any_of, expect_none_of, no_internals_leak,
               scenario_pass, latency_s), plus a run-level pass_rate
artifacts      report.json (schema_version 2, per-scenario latency + failure
               reason + trace id + dataset_run_url), summary.md (job summary,
               sticky PR comment), history.jsonl
dashboard      scripts/build_dashboard.py renders the whole history with a
               Langfuse link per run: https://dadadevelopment.github.io/agentrun/
gate           --fail-below <rate>, reading the same pass_rate Langfuse stored
```

Secrets: `MODEL_API_KEY`, `AGENT_REPO_TOKEN` (clone of the private agent repo),
`LANGFUSE_PUBLIC_KEY`, `LANGFUSE_SECRET_KEY`, `LANGFUSE_HOST`. Raising
`--fail-below` ratchets quality up as the prompt improves.

Adding a case is a YAML edit in `evals/`; the next CI run syncs it into the
dataset and scores it. Nothing is authored in the Langfuse UI, and nothing is
scored twice.

Use it from another repo's workflow:

```yaml
jobs:
  eval:
    uses: DadaDevelopment/agentrun/.github/workflows/agent-eval.yml@main
    with:
      label: pr
    secrets: inherit
```

