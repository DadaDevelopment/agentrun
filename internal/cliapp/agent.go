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
}

// consoleResolver resolves an agent's tools through the console API. It is the
// live implementation of agentrun.ToolResolver.
type consoleResolver struct {
	ctx    context.Context
	client *apiclient.Client
}

func (r consoleResolver) AgentTools(project, env, agent string) ([]agentspec.Tool, error) {
	ref, err := r.client.ResolveRef(r.ctx, project, env, "")
	if err != nil {
		return nil, err
	}
	found, err := r.client.AgentByName(r.ctx, ref.ProjectID, ref.EnvironmentID, agent)
	if err != nil {
		return nil, err
	}
	tools := make([]agentspec.Tool, 0, len(found.Tools))
	for _, t := range found.Tools {
		tools = append(tools, agentspec.Tool{URL: t.URL, Headers: t.Headers, Timeout: t.Timeout})
	}
	return tools, nil
}

func (r consoleResolver) AppURL(project, env, app string) (string, error) {
	ref, err := r.client.ResolveRef(r.ctx, project, env, app)
	if err != nil {
		return "", err
	}
	if ref.AppURL == "" {
		return "", fmt.Errorf("app %s has no public url", app)
	}
	return ref.AppURL, nil
}

func loadSpec(opts AgentOptions) (*agentspec.Spec, error) {
	return agentspec.Load(opts.Repo, opts.Agent)
}

// loadStateEnv reads .ddc/.env so a developer keeps model credentials next to
// the repo instead of exporting them by hand every session. Existing process
// variables win, and the file is never read into the console's audit trail.
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

// AgentSpec prints what the manifest resolves to, so a broken layout is visible
// before `up` fails obscurely.
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
	model := spec.Runtime.Model.Name
	if model == "" {
		model = "(default)"
	}
	fmt.Fprintf(out, "runtime    model=%s  tools=%d  image=%s\n", model, len(spec.Runtime.Tools), spec.Runtime.Image)
	fmt.Fprintf(out, "suites     %s\n", names(agentspec.List(spec.Suites, ".yaml")))
	fmt.Fprintf(out, "judges     %s\n", names(agentspec.List(spec.Judges, ".yaml")))
	if spec.Cases != "" {
		state := "ok"
		if _, err := os.Stat(spec.Cases); err != nil {
			state = "MISSING"
		}
		rel, _ := filepath.Rel(spec.Repo, spec.Cases)
		fmt.Fprintf(out, "cases      %s (%s, threshold %.2f)\n", rel, state, spec.HoldoutThreshold)
	}
	if spec.Console.Project != "" {
		fmt.Fprintf(out, "console    %s/%s\n", spec.Console.Project, spec.Console.Env)
	}
	fmt.Fprintf(out, "langfuse   dataset prefix %s\n", spec.DatasetPrefix())
	return nil
}

// AgentUp starts the agent from its spec.
func AgentUp(ctx context.Context, cfg Config, opts AgentOptions, out io.Writer) error {
	spec, err := loadSpec(opts)
	if err != nil {
		return err
	}
	loadStateEnv(spec.Repo)
	runOpts := agentrun.Options{
		Repo: spec.Repo, Agent: spec.Name, Prompt: opts.Prompt, MCP: opts.MCP, Port: opts.Port,
	}
	if len(opts.MCP) == 0 && len(spec.Runtime.Tools) == 0 && spec.Console.Project != "" {
		if err := EnsureLoggedIn(ctx, cfg, out); err != nil {
			return err
		}
		client := apiclient.New(cfg.APIBase, httpClient(), TokenSource(cfg), agentmarker.DetectFromEnv())
		runOpts.Resolver = consoleResolver{ctx: ctx, client: client}
	}
	return agentrun.Up(spec, runOpts, out)
}

// AgentDown stops the agent.
func AgentDown(opts AgentOptions, out io.Writer) error {
	spec, err := loadSpec(opts)
	if err != nil {
		return err
	}
	return agentrun.Down(spec, out)
}

// AgentLogs streams the agent's logs.
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
