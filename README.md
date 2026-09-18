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

There is no manifest to write. An agent repo is recognised by its layout, and
`ddc` is already signed in, so nothing in the repo has to name a project, an
environment or an agent:

```
agents/<name>/
  core.md              prompt, the file the prod runtime serves
  domains/*.md         skills, served through load_skill
  evals/suites/*.yaml  deterministic eval scenarios
  judge/*.yaml         llm-as-judge rules and prompts
```

Everything else has a default. `.dada/agent.json` is optional and holds only
what genuinely belongs to the repo rather than to a person - a pinned image,
non-default tools:

```json
{
  "agents": {
    "tg-exchange-support": {
      "image": "ghcr.io/dadadevelopment/kagent-app:v0.10.0-rc3-dada1",
      "model": { "name": "glm-5.3-flash", "api_key_env": "MODEL_API_KEY" },
      "tools": [{ "url": "https://tools.example/mcp" }]
    }
  }
}
```

Only the *name* of the key variable is stored. Secrets come from the
environment or `.ddc/.env` (gitignore `.ddc/`).

Where the agent is deployed is not in the repo either: it is asked once and
remembered per directory in your own config, exactly as `ddc deploy` remembers
an app's project. Two people can run the same repo against different
environments without editing a committed file.

```bash
cd my-agent-repo
ddc agent spec       # what the manifest resolves to - run this when a layout looks wrong
ddc agent up         # start the agent from the spec
ddc agent smoke      # one real A2A turn, exit 1 on silence
ddc agent eval       # run the repo's eval suites against it
ddc agent deploy --project <name> --env <name>   # asked once, then remembered
ddc agent logs -f
ddc agent down
```

### Deploy

`ddc agent deploy` reads the repo and calls the console's API. The dependency
points that way on purpose: production does not read files out of your
repository, so the layout above is ddc's contract rather than the platform's,
and the UI is skipped entirely.

CI runs the same two commands a developer runs - `ddc agent eval` and
`ddc agent deploy` - so a pipeline is never a separate code path. CI has no
browser, so it authenticates with `DDC_TOKEN`, or with `DDC_SERVICE_CLIENT_ID`
plus `DDC_SERVICE_CLIENT_SECRET` for the client-credentials grant.

A deploy is finished when its operation reaches `Committed`: the gitops agent
ends an agent write there and nothing advances that row afterwards.

Flags: `--repo`, `--agent` (when the manifest declares several), `--prompt` to
try another prompt without touching the spec, `--mcp URL` (repeatable) to
override the tool servers, `--port` (default 18081).

Tool priority: `--mcp`, then the tools in `.dada/agent.json`, then the console -
ddc asks the API which tools the agent runs with in its remembered environment
and rewrites in-cluster URLs to their public ones.

The agent runs in the production kagent image - the patched one from
[DadaDevelopment/kagent](https://github.com/DadaDevelopment/kagent), public so
no registry login is needed - so what runs locally is what runs in production,
tracing patch included. The rendered config is copied
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
