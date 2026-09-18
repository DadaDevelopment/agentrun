package cliapp

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dada-tuda/ddc/internal/agentmarker"
	"github.com/dada-tuda/ddc/internal/agentrun"
	"github.com/dada-tuda/ddc/internal/agentspec"
	"github.com/dada-tuda/ddc/internal/apiclient"
)

// AgentOptions are the shared flags of the `ddc agent` subcommands.
type AgentOptions struct {
	Repo    string
	Agent   string
	Prompt  string
	MCP     []string
	Port    int
	Text    string
	Follow  bool
	Tail    string
	Timeout time.Duration
	Project string
	Env     string
	Suite   string
	DryRun  bool
}

// consoleResolver resolves an agent's tools through the console API. It is the
// live implementation of agentrun.ToolResolver.
type consoleResolver struct {
	ctx     context.Context
	client  *apiclient.Client
	project string
	env     string
}

func (r consoleResolver) AgentTools(agent string) ([]agentspec.Tool, error) {
	ref, err := r.client.ResolveRef(r.ctx, r.project, r.env, "")
	if err != nil {
		return nil, err
	}
	found, err := r.client.AgentByName(r.ctx, ref.ProjectID, ref.EnvironmentID, agent)
	if err != nil {
		return nil, err
	}
	tools := make([]agentspec.Tool, 0, len(found.Tools))
	for _, t := range found.Tools {
		tools = append(tools, agentspec.Tool{Name: t.Name, URL: t.URL, Headers: t.Headers, Timeout: t.Timeout})
	}
	return tools, nil
}

func (r consoleResolver) AppURL(app string) (string, error) {
	ref, err := r.client.ResolveRef(r.ctx, r.project, r.env, app)
	if err != nil {
		return "", err
	}
	if ref.AppURL == "" {
		return "", fmt.Errorf("app %s has no public url", app)
	}
	return ref.AppURL, nil
}

func loadSpec(opts AgentOptions) (*agentspec.Spec, error) {
	return agentspec.Discover(opts.Repo, opts.Agent)
}

// loadStateEnv reads .ddc/.env so a developer keeps model credentials next to
// the repo instead of exporting them by hand every session. Existing process
// variables win.
func loadStateEnv(repo string) {
	raw, err := os.ReadFile(filepath.Join(repo, agentrun.StateDir, ".env"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		os.Setenv(key, strings.Trim(strings.TrimSpace(value), `"'`))
	}
}

// agentTarget resolves where this agent is deployed. The answer is remembered
// per directory in the user's own config, never committed to the repo, so the
// same source tree can be deployed to different environments by different
// people - the same rule `ddc deploy` already follows for apps.
func agentTarget(ctx context.Context, cfg Config, spec *agentspec.Spec, opts AgentOptions, out io.Writer) (Target, error) {
	if opts.Project != "" && opts.Env != "" {
		target := Target{ProjectName: opts.Project, EnvName: opts.Env, AppName: spec.Name}
		if err := RememberTarget(spec.Dir, target); err != nil {
			fmt.Fprintf(out, "note: could not remember the target: %v\n", err)
		}
		return target, nil
	}
	if target, ok, err := LookupTarget(spec.Dir); err == nil && ok {
		return target, nil
	}
	return Target{}, fmt.Errorf("where should %s be deployed? pass --project <name> --env <name> once; ddc remembers it for this directory", spec.Name)
}

// AgentSpec prints what the repo resolves to, so a broken layout is visible
// before `up` or `deploy` fails obscurely.
func AgentSpec(opts AgentOptions, out io.Writer) error {
	spec, err := loadSpec(opts)
	if err != nil {
		return err
	}
	prompt, err := os.ReadFile(spec.Prompt)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "agent      %s\n", spec.Name)
	fmt.Fprintf(out, "spec dir   %s\n", spec.Dir)
	fmt.Fprintf(out, "prompt     core.md  (%d chars)\n", len(prompt))
	fmt.Fprintf(out, "domains    %s\n", names(agentspec.List(spec.Domains, ".md")))
	fmt.Fprintf(out, "suites     %s\n", names(agentspec.List(spec.Suites, ".yaml")))
	fmt.Fprintf(out, "judges     %s\n", names(agentspec.List(spec.Judges, ".yaml")))
	model := spec.Runtime.Model.Name
	if model == "" {
		model = "(platform default)"
	}
	image := spec.Runtime.Image
	if image == "" {
		image = "(platform default)"
	}
	fmt.Fprintf(out, "runtime    model=%s  tools=%d  image=%s\n", model, len(spec.Runtime.Tools), image)
	if target, ok, err := LookupTarget(spec.Dir); err == nil && ok {
		fmt.Fprintf(out, "target     %s/%s (remembered)\n", target.ProjectName, target.EnvName)
	} else {
		fmt.Fprintf(out, "target     not set - pass --project/--env on the first deploy\n")
	}
	return nil
}

// AgentUp starts the agent locally from the repo.
func AgentUp(ctx context.Context, cfg Config, opts AgentOptions, out io.Writer) error {
	spec, err := loadSpec(opts)
	if err != nil {
		return err
	}
	loadStateEnv(spec.Repo)
	runOpts := agentrun.Options{
		Repo: spec.Repo, Agent: spec.Name, Prompt: opts.Prompt, MCP: opts.MCP, Port: opts.Port,
	}
	if len(opts.MCP) == 0 && len(spec.Runtime.Tools) == 0 {
		target, err := agentTarget(ctx, cfg, spec, opts, out)
		if err != nil {
			return fmt.Errorf("no tools to run with: declare them in %s, pass --mcp URL, or %w",
				agentspec.OverridePath, err)
		}
		client, err := agentClient(ctx, cfg, out)
		if err != nil {
			return err
		}
		runOpts.Resolver = consoleResolver{ctx: ctx, client: client, project: target.ProjectName, env: target.EnvName}
	}
	return agentrun.Up(spec, runOpts, out)
}

// agentClient signs in the way this environment allows: a machine credential
// when CI supplies one, the browser session otherwise.
func agentClient(ctx context.Context, cfg Config, out io.Writer) (*apiclient.Client, error) {
	if MachineCredentialsPresent() {
		token, err := machineTokenSource(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if token != "" {
			source := func(context.Context) (string, error) { return token, nil }
			return apiclient.New(cfg.APIBase, httpClient(), source, agentmarker.DetectFromEnv()), nil
		}
	}
	if err := EnsureLoggedIn(ctx, cfg, out); err != nil {
		return nil, err
	}
	return apiclient.New(cfg.APIBase, httpClient(), TokenSource(cfg), agentmarker.DetectFromEnv()), nil
}

// AgentDeploy ships the repo's agent to the platform.
//
// The direction of the dependency is the point: production does not read files
// out of this repo, ddc reads the repo and calls the console's API. The same
// command a developer runs by hand is what CI runs, so a deploy is never a
// different code path from a local one.
func AgentDeploy(ctx context.Context, cfg Config, opts AgentOptions, out io.Writer) error {
	spec, err := loadSpec(opts)
	if err != nil {
		return err
	}
	prompt, err := os.ReadFile(spec.Prompt)
	if err != nil {
		return err
	}
	target, err := agentTarget(ctx, cfg, spec, opts, out)
	if err != nil {
		return err
	}

	req := apiclient.SaveAgentRequest{
		Name:        spec.Name,
		Description: spec.Runtime.Description,
		Prompt:      string(prompt),
	}
	for _, t := range spec.Runtime.Tools {
		tool := apiclient.AgentToolSpec{Name: t.Name, URL: t.URL, AllowedHeaders: []string{"x-dada-end-user", "x-dada-agent"}}
		if tool.Name == "" {
			tool.Name = spec.Name + "-tools"
		}
		if t.Timeout > 0 {
			tool.Timeout = fmt.Sprintf("%ds", t.Timeout)
		}
		for name, value := range t.Headers {
			tool.Headers = append(tool.Headers, apiclient.AgentToolHeader{Name: name, Value: value})
		}
		req.Tools = append(req.Tools, tool)
	}

	fmt.Fprintf(out, "deploy %s -> %s/%s\n", spec.Name, target.ProjectName, target.EnvName)
	fmt.Fprintf(out, "  prompt %d chars, %d domains, %d tools\n",
		len(prompt), len(agentspec.List(spec.Domains, ".md")), len(req.Tools))
	if opts.DryRun {
		fmt.Fprintln(out, "dry run: nothing was sent")
		return nil
	}

	client, err := agentClient(ctx, cfg, out)
	if err != nil {
		return err
	}
	ref, err := client.ResolveRef(ctx, target.ProjectName, target.EnvName, "")
	if err != nil {
		return err
	}
	op, err := client.SaveAgent(ctx, ref.ProjectID, ref.EnvironmentID, req)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  operation %s\n", op.ID)
	final, err := client.WaitOperation(ctx, ref.ProjectID, op.ID, opts.Timeout, func(status string) {
		fmt.Fprintf(out, "  %s\n", status)
	})
	if err != nil {
		return err
	}
	if !final.OK() {
		return fmt.Errorf("deploy finished %s: %s", final.Status, final.Error)
	}
	fmt.Fprintln(out, "deployed")
	return nil
}

// AgentDown stops the local agent.
func AgentDown(opts AgentOptions, out io.Writer) error {
	spec, err := loadSpec(opts)
	if err != nil {
		return err
	}
	return agentrun.Down(spec, out)
}

// AgentLogs streams the local agent's logs.
func AgentLogs(opts AgentOptions, out io.Writer) error {
	spec, err := loadSpec(opts)
	if err != nil {
		return err
	}
	return agentrun.Logs(spec, opts.Follow, opts.Tail, out)
}

// AgentSmoke sends one real turn to the running agent.
func AgentSmoke(opts AgentOptions, out io.Writer) error {
	return agentrun.Smoke(opts.Port, opts.Text, opts.Timeout, out)
}

func names(paths []string) string {
	if len(paths) == 0 {
		return "0: -"
	}
	stems := make([]string, 0, len(paths))
	for _, p := range paths {
		stems = append(stems, strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)))
	}
	return fmt.Sprintf("%d: %s", len(stems), strings.Join(stems, ", "))
}
