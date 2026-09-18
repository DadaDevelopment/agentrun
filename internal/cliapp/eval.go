package cliapp

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dada-tuda/ddc/internal/agentrun"
	"github.com/dada-tuda/ddc/internal/agentspec"
)

// evalEntrypoint is the eval runner a repo is expected to ship. Scoring lives
// there rather than in ddc on purpose: the cases, the judges and the Langfuse
// client are the agent team's business, while ddc only guarantees that a local
// agent is running and that CI invokes exactly what a developer invokes.
const evalEntrypoint = "scripts/eval_run.py"

// extraEvalArgs are runner flags supplied through DDC_EVAL_ARGS.
//
// A pipeline needs to label a run, pick an environment and set a gate, and
// those belong to the repo's runner rather than to ddc: inventing a ddc flag
// per runner option would make ddc the place every eval convention has to be
// re-implemented. Quoting follows the shell's rules for simple quoted words.
func extraEvalArgs() []string {
	raw := strings.TrimSpace(os.Getenv("DDC_EVAL_ARGS"))
	if raw == "" {
		return nil
	}
	var args []string
	var current strings.Builder
	quote := rune(0)
	for _, r := range raw {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t' || r == '\n':
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args
}

// AgentEval runs the repo's eval suites against a locally running agent.
//
// It is the same command in CI and on a laptop, for the same reason `ddc agent
// deploy` is: a pipeline that runs something a developer cannot run by hand is
// a pipeline nobody can debug.
func AgentEval(ctx context.Context, cfg Config, opts AgentOptions, out io.Writer) error {
	spec, err := loadSpec(opts)
	if err != nil {
		return err
	}
	loadStateEnv(spec.Repo)

	runner := filepath.Join(spec.Repo, evalEntrypoint)
	if _, err := os.Stat(runner); err != nil {
		return fmt.Errorf("no eval runner at %s: this repo ships no evals", evalEntrypoint)
	}
	if len(agentspec.List(spec.Suites, ".yaml")) == 0 {
		return fmt.Errorf("no eval suites in %s", spec.Suites)
	}

	python := os.Getenv("DDC_PYTHON")
	if python == "" {
		python = "python3"
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/", opts.Port)
	if err := agentrun.Reachable(opts.Port); err != nil {
		return fmt.Errorf("%w: start it with `ddc agent up` first", err)
	}

	args := []string{runner, "--repo", spec.Repo, "--agent", spec.Name, "--url", url}
	if opts.Suite != "" {
		args = append(args, "--suite", opts.Suite)
	}
	args = append(args, extraEvalArgs()...)
	fmt.Fprintf(out, "eval %s via %s\n", spec.Name, evalEntrypoint)

	cmd := exec.CommandContext(ctx, python, args...)
	cmd.Dir = spec.Repo
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(spec.Repo, "scripts"))
	return cmd.Run()
}
