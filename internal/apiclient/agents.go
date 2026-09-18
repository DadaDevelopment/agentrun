package apiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Agent is one agent as the console knows it in an environment.
type Agent struct {
	Name  string
	Tools []AgentTool
}

// AgentTool is one MCP server an agent is wired to in the cluster.
type AgentTool struct {
	Name    string
	URL     string
	Timeout int
	Headers map[string]string
}

type agentsResponse struct {
	Items []struct {
		Name        string `json:"name"`
		SummaryJSON struct {
			Spec struct {
				Tools []struct {
					Name    string `json:"name"`
					URL     string `json:"url"`
					Timeout int    `json:"timeout"`
					Headers []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"headers"`
				} `json:"tools"`
			} `json:"spec"`
		} `json:"summary_json"`
	} `json:"items"`
}

type resolveResponse struct {
	Project struct {
		ID string `json:"id"`
	} `json:"project"`
	Environment struct {
		ID string `json:"id"`
	} `json:"environment"`
	App struct {
		URL string `json:"url"`
	} `json:"app"`
}

// Ref is a project/environment (and optionally app) resolved to ids and urls.
type Ref struct {
	ProjectID     string
	EnvironmentID string
	AppURL        string
}

// ResolveRef turns human names into the ids the rest of the API expects.
func (c *Client) ResolveRef(ctx context.Context, project, env, app string) (Ref, error) {
	query := url.Values{"project": {project}, "env": {env}}
	if app != "" {
		query.Set("app", app)
	}
	var payload resolveResponse
	if err := c.doJSON(ctx, "GET", "/resolve?"+query.Encode(), nil, "", &payload); err != nil {
		return Ref{}, err
	}
	return Ref{
		ProjectID:     payload.Project.ID,
		EnvironmentID: payload.Environment.ID,
		AppURL:        payload.App.URL,
	}, nil
}

// ListAgents returns the agents deployed in an environment, with the tool
// wiring the platform actually runs them with.
func (c *Client) ListAgents(ctx context.Context, projectID, envID string) ([]Agent, error) {
	var payload agentsResponse
	path := fmt.Sprintf("/projects/%s/environments/%s/agents", projectID, envID)
	if err := c.doJSON(ctx, "GET", path, nil, "", &payload); err != nil {
		return nil, err
	}
	agents := make([]Agent, 0, len(payload.Items))
	for _, item := range payload.Items {
		tools := make([]AgentTool, 0, len(item.SummaryJSON.Spec.Tools))
		for _, t := range item.SummaryJSON.Spec.Tools {
			headers := make(map[string]string, len(t.Headers))
			for _, h := range t.Headers {
				headers[h.Name] = h.Value
			}
			tools = append(tools, AgentTool{Name: t.Name, URL: t.URL, Timeout: t.Timeout, Headers: headers})
		}
		agents = append(agents, Agent{Name: item.Name, Tools: tools})
	}
	return agents, nil
}

// AgentByName returns one agent of an environment.
func (c *Client) AgentByName(ctx context.Context, projectID, envID, name string) (Agent, error) {
	agents, err := c.ListAgents(ctx, projectID, envID)
	if err != nil {
		return Agent{}, err
	}
	for _, a := range agents {
		if a.Name == name {
			return a, nil
		}
	}
	known := make([]string, 0, len(agents))
	for _, a := range agents {
		known = append(known, a.Name)
	}
	return Agent{}, fmt.Errorf("no agent %q in this environment (console knows: %s)", name, strings.Join(known, ", "))
}

var _ = json.Marshal
