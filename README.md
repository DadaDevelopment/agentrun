# agentrun

Run a production Dada agent on your laptop (or a CI box) straight from its agent
repo such as [tg-agent-tools](https://github.com/DadaDevelopment/tg-agent-tools).
Same image as prod, your prompt, the prod MCP tools.

The agent repo is the spec, and it already declares itself: `.dada/agent.json`
(the `AGENT-REPO-SPEC` manifest the console reads) names the agent, and
`agents/<name>/` holds everything about it. agentrun adds no manifest of its
own - it reads that one.

```
agents/<name>/
  core.md              prompt, the same file the prod runtime serves
  domains/*.md         skills, served through load_skill
  runtime.yaml         how to start it: model, tools, image
  evals/suites/*.yaml  deterministic marker scenarios
  judge/*.yaml         llm-as-judge rules and prompt
```

```
eval.py / persona_eval.py --url http://127.0.0.1:18081/   -> agent (A2A), like prod eval

agent    ghcr.io/kagent-dev/kagent/app (upstream, public), config.json rendered from the spec
tools    runtime.yaml tools / console (DADA_TOKEN) / --mcp URL, in that priority;
         in-cluster .svc.cluster.local tool URLs are rewritten to the app's public URL
```

## Install

```bash
uv tool install git+https://github.com/DadaDevelopment/agentrun
```

or `pipx install git+https://github.com/DadaDevelopment/agentrun`. Stdlib only,
Python >= 3.10. Prerequisites: Docker (compose v2). The kagent app image is
public, so no `docker login` is needed. On Apple Silicon the image runs under
amd64 emulation, first turn ~15 s.

## Agent repo setup

`.dada/agent.json` - the manifest (v1 fields keep working, v2 adds the rest):

```json
{
  "version": 2,
  "agents": [
    {
      "name": "tg-exchange-support",
      "agents_root": ".",
      "cases": "evals/referral/cases.jsonl",
      "holdout_threshold": 0.75,
      "runtime": "agents/tg-exchange-support/runtime.yaml",
      "suites": "agents/tg-exchange-support/evals/suites",
      "judges": "agents/tg-exchange-support/judge",
      "console": { "project": "agent-sandbox", "env": "prod" },
      "langfuse": { "dataset_prefix": "tg-exchange-support" }
    }
  ]
}
```

`agents/<name>/runtime.yaml` - what the spec never described before: model,
tools, image. Names of env vars only, never secret values:

```yaml
model:
  name: glm-5.3-flash
  base_url: https://api.z.ai/api/coding/paas/v4
  api_key_env: MODEL_API_KEY
  max_tokens: 2048
tools:
  - url: https://tg-agent-tools-624109.dada-tuda.ru/mcp
    # headers_env: TOOL_HEADERS      # "Name: value" per line, for private servers
image: ghcr.io/kagent-dev/kagent/app:0.10.0-rc3
```

`.agentrun.env` (gitignored) or the environment:

```bash
MODEL_API_KEY=...            # required, name configurable via model.api_key_env
MODEL=glm-5.3-flash          # optional override
MODEL_BASE_URL=...           # optional override
DADA_TOKEN=...               # only if runtime.yaml declares no tools
# or DADA_CLIENT_ID / DADA_CLIENT_SECRET for the client-credentials flow
```

## Run

```bash
cd tg-agent-tools
printf '.agentrun/\n.agentrun.env\n' >> .gitignore
agentrun spec                            # what the manifest resolves to
agentrun up                              # render + start
agentrun smoke                           # one real A2A turn, exit 1 on silence
python3 eval.py --url http://127.0.0.1:18081/
agentrun logs -f
agentrun down
```

`agentrun spec` prints the resolved prompt, domains, runtime, suites and judges -
run it first when a layout looks wrong, it fails loudly instead of `up` failing
obscurely.

Prompt edits need `agentrun up` again (config.json is rendered at `up`).
`agentrun up --core agents/<name>/experiments/foo.md` renders a different prompt
without touching the spec; `up --mcp http://.../mcp` overrides the tool list.
`--agent NAME` picks one when the manifest declares several.

## State

`.agentrun/` holds the rendered `config.json`, `agent-card.json`, env files
(mode 0600) and the compose `.env`. Delete it to start clean.

## CI

The workflow lives in the agent repo (`.github/workflows/eval.yml`), because the
cases, the judge and the threshold live there. One source of truth: the cases
are `agents/<name>/evals/suites/*.yaml`, they are synced into a Langfuse
dataset, and the experiment run produces the numbers that the CI gate and the
dashboard both read.

```
spec + up      the agent must resolve and answer at all, else the job fails early
sync dataset   scripts/eval_sync.py pushes the suites into a Langfuse dataset
               (idempotent, items keyed by scenario id)
experiment     scripts/eval_run.py runs dataset.run_experiment: every turn is a
               traced A2A generation, every assertion is a Langfuse score
artifacts      report.json (schema_version 2, per-scenario latency + failure
               reason + trace id + dataset_run_url), summary.md, history.jsonl
dashboard      scripts/eval_dashboard.py renders the history with a Langfuse
               link per run
gate           --fail-below <rate>, reading the same pass_rate Langfuse stored
```

### Langfuse naming contract

Several layers score the same agent into one Langfuse project, so they must not
average into one meaningless number:

```
environment   local | ci | production        (Langfuse(environment=...))
score name    suite.*     marker assertions from the eval suites
              turn.*      the runtime turn judge
              funnel.*    the funnel judge
dataset       <dataset_prefix>-<suite>
run_name      ci-<run_id>                    always unique
```

`run_name` must be unique per run: deleting dataset runs in Langfuse is
eventually consistent, and runs sharing a name show up merged for a while.
A managed evaluator should filter `environment = production`, or it burns quota
scoring CI traces.

Secrets: `MODEL_API_KEY`, `LANGFUSE_PUBLIC_KEY`, `LANGFUSE_SECRET_KEY`,
`LANGFUSE_HOST`. Raising `--fail-below` ratchets quality up as the prompt
improves. Adding a case is a YAML edit in the suite; the next CI run syncs it
into the dataset and scores it. Nothing is authored in the Langfuse UI.
