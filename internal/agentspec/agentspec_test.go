package agentspec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDiscoverNeedsNoManifest is the point of this package: a repo that just
// follows the layout works with ddc the moment it is cloned.
func TestDiscoverNeedsNoManifest(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "agents", "roman", "core.md"), "be helpful")
	write(t, filepath.Join(dir, "agents", "roman", "domains", "pricing.md"), "prices")
	write(t, filepath.Join(dir, "agents", "roman", "evals", "suites", "markers.yaml"), "cases: []")
	write(t, filepath.Join(dir, "agents", "roman", "judge", "turn.yaml"), "rules: []")

	spec, err := Discover(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "roman" {
		t.Fatalf("name = %q", spec.Name)
	}
	if len(List(spec.Domains, ".md")) != 1 {
		t.Fatalf("domains = %v", List(spec.Domains, ".md"))
	}
	if len(List(spec.Suites, ".yaml")) != 1 {
		t.Fatalf("suites = %v", List(spec.Suites, ".yaml"))
	}
	if len(List(spec.Judges, ".yaml")) != 1 {
		t.Fatalf("judges = %v", List(spec.Judges, ".yaml"))
	}
	if spec.Runtime.Image != "" || spec.Runtime.Model.Name != "" {
		t.Fatalf("runtime should be empty without an override: %+v", spec.Runtime)
	}
}

func TestDiscoverReadsOptionalOverride(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "agents", "roman", "core.md"), "p")
	write(t, filepath.Join(dir, OverridePath), `{"agents":{"roman":{"image":"img:1",
"model":{"name":"m","api_key_env":"K"},"tools":[{"url":"https://x/mcp","timeout":9}]}}}`)

	spec, err := Discover(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Runtime.Image != "img:1" || spec.Runtime.Model.Name != "m" {
		t.Fatalf("runtime = %+v", spec.Runtime)
	}
	if len(spec.Runtime.Tools) != 1 || spec.Runtime.Tools[0].Timeout != 9 {
		t.Fatalf("tools = %+v", spec.Runtime.Tools)
	}
}

func TestDiscoverIgnoresOverrideForAnotherAgent(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "agents", "roman", "core.md"), "p")
	write(t, filepath.Join(dir, OverridePath), `{"agents":{"other":{"image":"nope"}}}`)
	spec, err := Discover(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Runtime.Image != "" {
		t.Fatalf("image = %q, another agent's override leaked", spec.Runtime.Image)
	}
}

func TestDiscoverRequiresANameWhenAmbiguous(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "agents", "a", "core.md"), "p")
	write(t, filepath.Join(dir, "agents", "b", "core.md"), "p")
	if _, err := Discover(dir, ""); err == nil || !strings.Contains(err.Error(), "--agent") {
		t.Fatalf("err = %v", err)
	}
	spec, err := Discover(dir, "b")
	if err != nil || spec.Name != "b" {
		t.Fatalf("spec = %+v err = %v", spec, err)
	}
	if _, err := Discover(dir, "missing"); err == nil {
		t.Fatal("an unknown agent name must fail")
	}
}

func TestDiscoverReportsAnEmptyRepo(t *testing.T) {
	if _, err := Discover(t.TempDir(), ""); err == nil || !strings.Contains(err.Error(), "core.md") {
		t.Fatalf("err = %v", err)
	}
}

func TestDiscoverSkipsDirectoriesWithoutAPrompt(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "agents", "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "agents", "real", "core.md"), "p")
	spec, err := Discover(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "real" {
		t.Fatalf("name = %q", spec.Name)
	}
}
