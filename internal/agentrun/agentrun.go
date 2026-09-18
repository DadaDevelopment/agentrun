// Package agentrun starts one agent from its repo spec on a laptop or a CI box.
//
// It renders the same config.json the kagent controller renders into the agent
// Secret in production, then runs the production agent image against it, so the
// model client, the MCP toolset and the A2A server are the prod ones. There is
// no compose file and no second manifest: the container is started with one
// docker run, and everything it needs comes from .dada/agent.json.
package agentrun

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dada-tuda/ddc/internal/agentspec"
)

// StateDir is where the rendered config lands inside the agent repo.
const StateDir = ".ddc"

const (
	defaultImage     = "ghcr.io/kagent-dev/kagent/app:0.10.0-rc3"
	defaultModel     = "glm-5.3-flash"
	defaultBaseURL   = "https://api.z.ai/api/coding/paas/v4"
	defaultMaxTokens = 2048
	containerPort    = "8080"
)

// Options are the knobs of `ddc agent up`.
type Options struct {
	Repo     string
	Agent    string
	Prompt   string
	MCP      []string
	Port     int
	Resolver ToolResolver
}

func containerName(agent string) string { return "ddc-agent-" + agent }

// Render writes config.json and agent-card.json for the agent and returns the
// state directory holding them.
func Render(spec *agentspec.Spec, opts Options, out io.Writer) (string, error) {
	model := spec.Runtime.Model
	keyEnv := model.APIKeyEnv
	if keyEnv == "" {
		keyEnv = "MODEL_API_KEY"
	}
	apiKey := os.Getenv(keyEnv)
	if apiKey == "" {
		return "", fmt.Errorf("%s is not set (export it or put it in %s/.env)", keyEnv, StateDir)
	}

	promptPath := spec.Prompt
	if opts.Prompt != "" {
		abs, err := filepath.Abs(opts.Prompt)
		if err != nil {
			return "", err
		}
		promptPath = abs
	}
	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		return "", fmt.Errorf("prompt: %w", err)
	}

	tools, err := resolveTools(spec, opts)
	if err != nil {
		return "", err
	}

	name := valueOr(model.Name, defaultModel)
	if env := os.Getenv("MODEL"); env != "" {
		name = env
	}
	baseURL := valueOr(model.BaseURL, defaultBaseURL)
	if env := os.Getenv("MODEL_BASE_URL"); env != "" {
		baseURL = env
	}
	maxTokens := model.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}

	httpTools := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		timeout := t.Timeout
		if timeout == 0 {
			timeout = 30
		}
		headers := t.Headers
		if headers == nil {
			headers = map[string]string{}
		}
		httpTools = append(httpTools, map[string]any{
			"params": map[string]any{
				"url": t.URL, "headers": headers, "timeout": timeout, "terminate_on_close": true,
			},
			"allowed_headers": []string{"x-dada-end-user", "x-dada-agent"},
		})
	}

	config := map[string]any{
		"model": map[string]any{
			"type":             "openai",
			"model":            name,
			"base_url":         baseURL,
			"max_tokens":       maxTokens,
			"reasoning_effort": valueOr(model.ReasoningEffort, "low"),
			"api_format":       valueOr(model.APIFormat, "chatCompletions"),
		},
		"description": valueOr(spec.Runtime.Description, spec.Name),
		"instruction": string(prompt),
		"http_tools":  httpTools,
		"stream":      false,
	}

	a2a := "http://127.0.0.1:" + containerPort
	card := map[string]any{
		"name":               strings.ReplaceAll(spec.Name, "-", "_"),
		"description":        config["description"],
		"version":            "",
		"skills":             []any{},
		"defaultInputModes":  []string{"text"},
		"defaultOutputModes": []string{"text"},
		"capabilities":       map[string]any{"streaming": true},
		"url":                a2a,
		"protocolVersion":    "0.3",
		"preferredTransport": "JSONRPC",
		"supportedInterfaces": []map[string]any{
			{"url": a2a, "protocolBinding": "JSONRPC", "protocolVersion": "0.3"},
			{"url": a2a, "protocolBinding": "JSONRPC", "protocolVersion": "1.0"},
		},
	}

	state := filepath.Join(spec.Repo, StateDir, "agent")
	if err := os.MkdirAll(state, 0o755); err != nil {
		return "", err
	}
	if err := writeJSON(filepath.Join(state, "config.json"), config); err != nil {
		return "", err
	}
	if err := writeJSON(filepath.Join(state, "agent-card.json"), card); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(state, "local_agent.py"), localAgentPy, 0o644); err != nil {
		return "", err
	}

	shown := promptPath
	if rel, err := filepath.Rel(spec.Repo, promptPath); err == nil && !strings.HasPrefix(rel, "..") {
		shown = rel
	}
	fmt.Fprintf(out, "agent %s  prompt %s  model %s\n", spec.Name, shown, name)
	for _, t := range tools {
		extra := ""
		if len(t.Headers) > 0 {
			extra = "  (+headers)"
		}
		fmt.Fprintf(out, "tool  %s%s\n", t.URL, extra)
	}
	return state, nil
}

// Up renders the config and starts the agent container, waiting for health.
func Up(spec *agentspec.Spec, opts Options, out io.Writer) error {
	state, err := Render(spec, opts, out)
	if err != nil {
		return err
	}
	keyEnv := valueOr(spec.Runtime.Model.APIKeyEnv, "MODEL_API_KEY")
	image := valueOr(spec.Runtime.Image, defaultImage)
	if env := os.Getenv("AGENT_IMAGE"); env != "" {
		image = env
	}
	name := containerName(spec.Name)
	_ = exec.Command("docker", "rm", "-f", name).Run()

	// The config is copied into the container rather than bind-mounted: the
	// docker daemon may not share this filesystem (remote daemon, docker-in-
	// docker, a CI runner), and a bind mount would then silently mount an
	// empty directory and the agent would exit before it ever served a turn.
	args := []string{
		"create", "--name", name,
		"--platform", "linux/amd64",
		"-p", fmt.Sprintf("127.0.0.1:%d:%s", opts.Port, containerPort),
		"-e", "AGENTRUN_CONFIG=/config",
		"-e", "OPENAI_API_KEY=" + os.Getenv(keyEnv),
		"-e", "KAGENT_NAME=" + spec.Name,
		"-e", "KAGENT_NAMESPACE=kagent",
		"-e", "KAGENT_URL=http://127.0.0.1:8083",
		"-e", "OTEL_TRACING_ENABLED=false",
		"-e", "OTEL_LOGGING_ENABLED=false",
		"--add-host", "host.docker.internal:host-gateway",
		"--entrypoint", "/.kagent/.venv/bin/python",
		image, "/config/local_agent.py",
	}
	create := exec.Command("docker", args...)
	create.Stderr = out
	if err := create.Run(); err != nil {
		return fmt.Errorf("docker create: %w", err)
	}
	copyIn := exec.Command("docker", "cp", state+"/.", name+":/config")
	copyIn.Stderr = out
	if err := copyIn.Run(); err != nil {
		return fmt.Errorf("docker cp config: %w", err)
	}
	start := exec.Command("docker", "start", name)
	start.Stderr = out
	if err := start.Run(); err != nil {
		return fmt.Errorf("docker start: %w", err)
	}
	if err := waitHealthy(name, opts.Port, 180*time.Second); err != nil {
		logs, _ := exec.Command("docker", "logs", "--tail", "40", name).CombinedOutput()
		out.Write(logs)
		return err
	}
	fmt.Fprintf(out, "up: agent A2A http://127.0.0.1:%d/\n", opts.Port)
	return nil
}

// Down removes the agent container.
func Down(spec *agentspec.Spec, out io.Writer) error {
	cmd := exec.Command("docker", "rm", "-f", containerName(spec.Name))
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

// Logs streams the agent container's logs.
func Logs(spec *agentspec.Spec, follow bool, tail string, out io.Writer) error {
	args := []string{"logs", "--tail", tail}
	if follow {
		args = append(args, "-f")
	}
	cmd := exec.Command("docker", append(args, containerName(spec.Name))...)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

// waitHealthy blocks until the agent answers its own health endpoint. An open
// TCP port is not the signal: docker publishes the port before the process
// inside is listening and keeps it during teardown, so dialling it reports a
// dead agent as healthy.
func waitHealthy(container string, port int, limit time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", container).Output()
		if err == nil && strings.TrimSpace(string(out)) != "true" {
			return fmt.Errorf("the agent container exited before it became healthy")
		}
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("the agent did not report healthy on port %d within %s", port, limit)
}

// writeJSON renders the config the agent reads. The file is world-readable on
// purpose: the agent image runs as a non-root user, and 0600 would leave it
// unable to read its own configuration. No secret is written here - the model
// key reaches the container through the environment, never the config file.
func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
