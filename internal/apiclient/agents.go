package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
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

// AgentToolHeader is one header attached to a tool call.
type AgentToolHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// AgentToolSpec is one tool as sent to the console when saving an agent.
type AgentToolSpec struct {
	Name           string            `json:"name"`
	URL            string            `json:"url,omitempty"`
	Description    string            `json:"description,omitempty"`
	Timeout        string            `json:"timeout,omitempty"`
	Protocol       string            `json:"protocol,omitempty"`
	Headers        []AgentToolHeader `json:"headers,omitempty"`
	AllowedHeaders []string          `json:"allowed_headers,omitempty"`
}

// SaveAgentRequest is the console's create-or-update contract. A field left
// empty keeps its current value, so a prompt-only deploy does not drop the
// model, the runtime or the tools.
type SaveAgentRequest struct {
	Name          string          `json:"name"`
	DisplayName   string          `json:"display_name,omitempty"`
	Description   string          `json:"description,omitempty"`
	Prompt        string          `json:"prompt,omitempty"`
	PromptVersion string          `json:"prompt_version,omitempty"`
	ModelConfig   string          `json:"model_config,omitempty"`
	Runtime       string          `json:"runtime,omitempty"`
	Tools         []AgentToolSpec `json:"tools,omitempty"`
}

// Operation is an async platform write. The console never commits to the
// infrastructure repo inside the request: it queues an operation the gitops
// agent renders and commits, so a deploy is finished when this reaches a
// terminal status, not when the POST returns.
type Operation struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error_message"`
}

// Done reports whether the operation will not change any further.
//
// Committed counts as terminal, not as a stage on the way to Ready: the gitops
// agent ends an agent write at Committed and nothing advances that row
// afterwards. Waiting for Ready therefore hangs until the timeout on a deploy
// that already succeeded. This mirrors classifyOperationStatus in the console,
// which is the platform's own definition.
func (o Operation) Done() bool {
	switch o.Status {
	case "Committed", "Ready", "Failed", "Cancelled":
		return true
	}
	return false
}

// OK reports whether the operation finished successfully.
func (o Operation) OK() bool {
	return o.Status == "Committed" || o.Status == "Ready"
}

// SaveAgent creates or updates an agent and returns the queued operation.
func (c *Client) SaveAgent(ctx context.Context, projectID, envID string, req SaveAgentRequest) (Operation, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Operation{}, err
	}
	var payload struct {
		Operation Operation `json:"operation"`
	}
	path := fmt.Sprintf("/projects/%s/environments/%s/agents", projectID, envID)
	if err := c.doJSON(ctx, "POST", path, bytes.NewReader(body), "application/json", &payload); err != nil {
		return Operation{}, err
	}
	return payload.Operation, nil
}

// GetOperation reads one operation's current state.
func (c *Client) GetOperation(ctx context.Context, projectID, operationID string) (Operation, error) {
	var payload struct {
		Operation Operation `json:"operation"`
	}
	path := fmt.Sprintf("/projects/%s/operations/%s", projectID, operationID)
	if err := c.doJSON(ctx, "GET", path, nil, "", &payload); err != nil {
		return Operation{}, err
	}
	if payload.Operation.ID == "" {
		var direct Operation
		if err := c.doJSON(ctx, "GET", path, nil, "", &direct); err != nil {
			return Operation{}, err
		}
		return direct, nil
	}
	return payload.Operation, nil
}

// WaitOperation polls until the operation reaches a terminal status. Each
// observed status is reported through onStatus so a CLI can show progress.
func (c *Client) WaitOperation(ctx context.Context, projectID, operationID string, limit time.Duration, onStatus func(string)) (Operation, error) {
	deadline := time.Now().Add(limit)
	last := ""
	for time.Now().Before(deadline) {
		op, err := c.GetOperation(ctx, projectID, operationID)
		if err != nil {
			return Operation{}, err
		}
		if op.Status != last && onStatus != nil {
			onStatus(op.Status)
			last = op.Status
		}
		if op.Done() {
			return op, nil
		}
		select {
		case <-ctx.Done():
			return Operation{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return Operation{}, fmt.Errorf("operation %s did not finish within %s (last status %q)", operationID, limit, last)
}
