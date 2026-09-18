package agentrun

import (
	_ "embed"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/dada-tuda/ddc/internal/agentspec"
)

// localAgentPy serves one agent from the mounted config dir inside the prod
// image. It is embedded so `ddc agent up` needs nothing but the binary.
//
//go:embed local_agent.py
var localAgentPy []byte

// ToolResolver asks the console which tools an agent runs with. cliapp supplies
// the real one; keeping it an interface here means this package never reaches
// for a session or an HTTP client of its own.
type ToolResolver interface {
	AgentTools(project, env, agent string) ([]agentspec.Tool, error)
	AppURL(project, env, app string) (string, error)
}

// resolveTools decides which MCP servers the agent may call, in priority order:
// the --mcp flag, then the tools declared in the manifest, then the console.
func resolveTools(spec *agentspec.Spec, opts Options) ([]agentspec.Tool, error) {
	if len(opts.MCP) > 0 {
		tools := make([]agentspec.Tool, 0, len(opts.MCP))
		for _, u := range opts.MCP {
			tools = append(tools, agentspec.Tool{URL: u})
		}
		return tools, nil
	}
	if len(spec.Runtime.Tools) > 0 {
		tools := make([]agentspec.Tool, 0, len(spec.Runtime.Tools))
		for _, t := range spec.Runtime.Tools {
			headers, err := toolHeaders(t)
			if err != nil {
				return nil, err
			}
			tools = append(tools, agentspec.Tool{URL: t.URL, Headers: headers, Timeout: t.Timeout})
		}
		return tools, nil
	}
	if opts.Resolver == nil {
		return nil, fmt.Errorf("no tools: declare runtime.tools in %s or pass --mcp URL", agentspec.ManifestPath)
	}
	return consoleTools(spec, opts.Resolver)
}

// toolHeaders merges literal headers with any carried in an environment
// variable, so a private MCP server's credentials stay out of the repo.
func toolHeaders(t agentspec.Tool) (map[string]string, error) {
	headers := map[string]string{}
	for k, v := range t.Headers {
		headers[k] = v
	}
	if t.HeadersEnv == "" {
		return headers, nil
	}
	raw := os.Getenv(t.HeadersEnv)
	if raw == "" {
		return nil, fmt.Errorf("tool %s declares headers_env %s but it is not set", t.URL, t.HeadersEnv)
	}
	for _, line := range strings.Split(raw, "\n") {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	return headers, nil
}

// consoleTools asks the console which tools this agent runs with in its
// environment, rewriting in-cluster URLs to the app's public URL.
func consoleTools(spec *agentspec.Spec, resolver ToolResolver) ([]agentspec.Tool, error) {
	if spec.Console.Project == "" || spec.Console.Env == "" {
		return nil, fmt.Errorf("no tools: declare runtime.tools in %s, pass --mcp, or set console.project/env",
			agentspec.ManifestPath)
	}
	tools, err := resolver.AgentTools(spec.Console.Project, spec.Console.Env, spec.Name)
	if err != nil {
		return nil, err
	}
	out := make([]agentspec.Tool, 0, len(tools))
	for _, t := range tools {
		u, err := url.Parse(t.URL)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(u.Hostname(), ".svc.cluster.local") {
			app := strings.TrimSuffix(strings.Split(u.Hostname(), ".")[0], "-service")
			public, err := resolver.AppURL(spec.Console.Project, spec.Console.Env, app)
			if err != nil {
				return nil, fmt.Errorf("tool %s: %w", t.URL, err)
			}
			t.URL = strings.TrimRight(public, "/") + u.Path
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the console lists no tools for %s; declare runtime.tools in %s",
			spec.Name, agentspec.ManifestPath)
	}
	return out, nil
}
