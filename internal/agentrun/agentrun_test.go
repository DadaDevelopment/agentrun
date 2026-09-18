package agentrun

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dada-tuda/ddc/internal/agentspec"
)

func repo(t *testing.T, manifest string) *agentspec.Spec {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, agentspec.ManifestPath), manifest)
	mustWrite(t, filepath.Join(dir, "agents", "a", "core.md"), "be helpful")
	spec, err := agentspec.Load(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const withTool = `{"version":1,"agents":[{"name":"a","runtime":{"model":{"name":"m","api_key_env":"TEST_KEY"},
"tools":[{"url":"https://tools/mcp","timeout":7}]}}]}`

// TestRenderWritesConfigTheAgentCanRead guards two startup failures seen live:
// a 0600 config the non-root container user could not read, and the model key
// leaking into a file when it belongs in the environment.
func TestRenderWritesConfigTheAgentCanRead(t *testing.T) {
	t.Setenv("TEST_KEY", "secret-value")
	spec := repo(t, withTool)
	state, err := Render(spec, Options{Port: 1}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "config.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o044 == 0 {
		t.Fatalf("config.json mode %v is unreadable for the container user", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-value") {
		t.Fatal("the model key leaked into config.json; it must travel in the environment")
	}
	var config struct {
		Model struct {
			Model string `json:"model"`
		} `json:"model"`
		Instruction string `json:"instruction"`
		HTTPTools   []struct {
			Params struct {
				URL     string `json:"url"`
				Timeout int    `json:"timeout"`
			} `json:"params"`
		} `json:"http_tools"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if config.Model.Model != "m" || config.Instruction != "be helpful" {
		t.Fatalf("config = %+v", config)
	}
	if len(config.HTTPTools) != 1 || config.HTTPTools[0].Params.URL != "https://tools/mcp" {
		t.Fatalf("tools = %+v", config.HTTPTools)
	}
	if config.HTTPTools[0].Params.Timeout != 7 {
		t.Fatalf("timeout = %d", config.HTTPTools[0].Params.Timeout)
	}
}

func TestRenderRefusesWithoutModelKey(t *testing.T) {
	t.Setenv("TEST_KEY", "")
	spec := repo(t, withTool)
	if _, err := Render(spec, Options{Port: 1}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "TEST_KEY") {
		t.Fatalf("err = %v", err)
	}
}

func TestRenderPrefersFlagOverDeclaredTools(t *testing.T) {
	t.Setenv("TEST_KEY", "k")
	spec := repo(t, withTool)
	state, err := Render(spec, Options{Port: 1, MCP: []string{"http://localhost:9/mcp"}}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(state, "config.json"))
	if !strings.Contains(string(raw), "http://localhost:9/mcp") ||
		strings.Contains(string(raw), "https://tools/mcp") {
		t.Fatal("--mcp did not override the declared tools")
	}
}

func TestToolHeadersReadEnvironment(t *testing.T) {
	t.Setenv("HDRS", "Authorization: Bearer abc\nX-Trace: on")
	headers, err := toolHeaders(agentspec.Tool{URL: "u", HeadersEnv: "HDRS"})
	if err != nil {
		t.Fatal(err)
	}
	if headers["Authorization"] != "Bearer abc" || headers["X-Trace"] != "on" {
		t.Fatalf("headers = %v", headers)
	}
	if _, err := toolHeaders(agentspec.Tool{URL: "u", HeadersEnv: "MISSING_HDRS"}); err == nil {
		t.Fatal("a declared headers_env that is unset must fail loudly")
	}
}

// TestWaitHealthyRejectsAnOpenPortThatIsNotTheAgent pins the bug where an open
// TCP port counted as healthy, so `up` reported success for a container that
// had already exited. Health is the agent's own /health answering 200.
func TestWaitHealthyRejectsAnOpenPortThatIsNotTheAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	port, err := strconv.Atoi(strings.Split(server.URL, ":")[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := waitHealthy("no-such-container", port, 2*time.Second); err == nil {
		t.Fatal("a listening port that never returns 200 must not count as healthy")
	}
}

func TestSmokeFailsOnEmptyAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"artifacts":[]}}`))
	}))
	defer server.Close()
	port, _ := strconv.Atoi(strings.Split(server.URL, ":")[2])
	if err := Smoke(port, "hi", 5*time.Second, &bytes.Buffer{}); err == nil {
		t.Fatal("transport 200 with no text is a failed turn, not a pass")
	}
}

func TestSmokePassesOnRealText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"result":{"artifacts":[{"parts":[{"text":"hello"}]}]}}`))
	}))
	defer server.Close()
	port, _ := strconv.Atoi(strings.Split(server.URL, ":")[2])
	out := &bytes.Buffer{}
	if err := Smoke(port, "hi", 5*time.Second, out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "smoke: OK") {
		t.Fatalf("out = %q", out.String())
	}
}
