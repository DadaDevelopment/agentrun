# ddc

The Dada Cloud CLI. One binary, two jobs: deploy an app, and run the agent a
repo describes.

```
usage: ddc <command>

commands:
  login            sign in via your browser (device code flow)
  deploy [dir]     package and deploy dir (default: current directory)
  agent <action>   run the agent described by this repo's .dada/agent.json
```

This repo is public on purpose: it is the only part of Dada Cloud users install
on their own machines, so the console can stay private without breaking
`install.sh`.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/DadaDevelopment/ddc/main/install.sh | sh
```

Go 1.25, no third-party dependencies. `ddc agent` additionally needs Docker.

## ddc agent

An agent repo already declares itself in `.dada/agent.json` - the manifest the
console reads. `ddc agent` reads the same file, so there is no second manifest
to drift:

```
agents/<name>/
  core.md              prompt, the file the prod runtime serves
  domains/*.md         skills, served through load_skill
  evals/suites/*.yaml  deterministic eval scenarios
  judge/*.yaml         llm-as-judge rules and prompts
```

The manifest adds how to run it, so local, CI and prod start the agent from one
description. `version` stays `1`: the console's reader accepts only version 1
and ignores keys it does not know, so these fields are additive.

```json
{
  "version": 1,
  "agents": [
    {
      "name": "tg-exchange-support",
      "agents_root": ".",
      "cases": "evals/referral/cases.jsonl",
      "holdout_threshold": 0.75,
      "runtime": {
        "image": "ghcr.io/kagent-dev/kagent/app:0.10.0-rc3",
        "model": {
          "name": "glm-5.3-flash",
          "base_url": "https://api.z.ai/api/coding/paas/v4",
          "api_key_env": "MODEL_API_KEY"
        },
        "tools": [{ "url": "https://tools.example/mcp" }]
      },
      "console": { "project": "agent-sandbox", "env": "prod" },
      "langfuse": { "dataset_prefix": "tg-exchange-support" }
    }
  ]
}
```

Only the *name* of the key variable is stored. Secrets come from the
environment or `.ddc/.env` (gitignore `.ddc/`).

```bash
cd my-agent-repo
ddc agent spec       # what the manifest resolves to - run this when a layout looks wrong
ddc agent up         # start the agent from the spec
ddc agent smoke      # one real A2A turn, exit 1 on silence
ddc agent logs -f
ddc agent down
```

Flags: `--repo`, `--agent` (when the manifest declares several), `--prompt` to
try another prompt without touching the spec, `--mcp URL` (repeatable) to
override the tool servers, `--port` (default 18081).

Tool priority: `--mcp`, then `runtime.tools`, then the console - with
`console.project`/`env` set, `ddc` asks the API which tools the agent runs with
in production and rewrites in-cluster URLs to their public ones.

The agent runs in the production kagent image, so the model client, the MCP
toolset and the A2A server are the prod ones. The rendered config is copied
into the container rather than bind-mounted, so a remote or docker-in-docker
daemon works the same. `up` returns only once the agent answers `/health`.

## Langfuse naming contract

Several layers score the same agent into one project, so they must not average
into one meaningless number:

```
environment   local | ci | production
score name    suite.*     marker assertions from the eval suites
              turn.*      the runtime turn judge
              funnel.*    the funnel judge
dataset       <dataset_prefix>-<suite>
run_name      ci-<run_id>                    always unique
```

`run_name` must be unique per run: deleting dataset runs is eventually
consistent, and runs sharing a name appear merged for a while. A managed
evaluator should filter `environment = production`, or it burns quota scoring
CI traces.

The eval runner, the dataset sync and the dashboard live in the agent repo,
next to the cases and the judge they belong to.
