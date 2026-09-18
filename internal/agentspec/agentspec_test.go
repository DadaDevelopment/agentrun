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

func repoWith(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, ManifestPath), manifest)
	write(t, filepath.Join(dir, "agents", "a", "core.md"), "prompt")
	return dir
}

const oneAgent = `{"version":1,"agents":[{"name":"a","agents_root":".","cases":"c.jsonl",
"holdout_threshold":0.75,"runtime":{"image":"img","model":{"name":"m","api_key_env":"K"},
"tools":[{"url":"https://x/mcp"}]},"console":{"project":"p","env":"prod"},
"langfuse":{"dataset_prefix":"pre"}}]}`

func TestLoadResolvesDeclaredLayout(t *testing.T) {
	dir := repoWith(t, oneAgent)
	spec, err := Load(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "a" {
		t.Fatalf("name = %q", spec.Name)
	}
	if spec.Runtime.Image != "img" || spec.Runtime.Model.Name != "m" {
		t.Fatalf("runtime not read: %+v", spec.Runtime)
	}
	if len(spec.Runtime.Tools) != 1 || spec.Runtime.Tools[0].URL != "https://x/mcp" {
		t.Fatalf("tools not read: %+v", spec.Runtime.Tools)
	}
	if spec.DatasetPrefix() != "pre" {
		t.Fatalf("prefix = %q", spec.DatasetPrefix())
	}
	if !strings.HasSuffix(spec.Suites, filepath.Join("agents", "a", "evals", "suites")) {
		t.Fatalf("suites = %q", spec.Suites)
	}
	if !strings.HasSuffix(spec.Judges, filepath.Join("agents", "a", "judge")) {
		t.Fatalf("judges = %q", spec.Judges)
	}
}

// The console's reader accepts only version 1, so ddc must refuse anything else
// rather than write a manifest the platform cannot read.
func TestLoadRejectsOtherVersions(t *testing.T) {
	dir := repoWith(t, `{"version":2,"agents":[{"name":"a"}]}`)
	if _, err := Load(dir, ""); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRequiresAgentNameWhenAmbiguous(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ManifestPath),
		`{"version":1,"agents":[{"name":"a"},{"name":"b"}]}`)
	write(t, filepath.Join(dir, "agents", "a", "core.md"), "p")
	write(t, filepath.Join(dir, "agents", "b", "core.md"), "p")
	if _, err := Load(dir, ""); err == nil || !strings.Contains(err.Error(), "--agent") {
		t.Fatalf("err = %v", err)
	}
	spec, err := Load(dir, "b")
	if err != nil || spec.Name != "b" {
		t.Fatalf("spec = %+v err = %v", spec, err)
	}
}

func TestLoadReportsMissingManifestAndPrompt(t *testing.T) {
	if _, err := Load(t.TempDir(), ""); err == nil || !strings.Contains(err.Error(), "declares no agent") {
		t.Fatalf("err = %v", err)
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, ManifestPath), `{"version":1,"agents":[{"name":"a"}]}`)
	if _, err := Load(dir, ""); err == nil || !strings.Contains(err.Error(), "core.md") {
		t.Fatalf("err = %v", err)
	}
}

func TestDatasetPrefixFallsBackToName(t *testing.T) {
	dir := repoWith(t, `{"version":1,"agents":[{"name":"a"}]}`)
	spec, err := Load(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if spec.DatasetPrefix() != "a" {
		t.Fatalf("prefix = %q", spec.DatasetPrefix())
	}
}
