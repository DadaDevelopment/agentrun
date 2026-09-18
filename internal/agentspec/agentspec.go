// Package agentspec reads the agent manifest a repo already ships, `.dada/agent.json`,
// and resolves every path that describes one agent: prompt, skills, runtime, eval
// suites and judges.
//
// The manifest is the only file that knows an agent's name, so a rename is one edit.
// Its version stays 1: the console's own reader (agentkit/repospec.py) accepts only
// version 1 and ignores keys it does not know, so the fields ddc needs are additive
// rather than a fork of the format.
package agentspec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ManifestPath is the manifest's location inside an agent repo.
const ManifestPath = ".dada/agent.json"

// Model is how to call the LLM. Only the name of the API key variable is stored;
// a secret never belongs in a file that is committed.
type Model struct {
	Name            string `json:"name"`
	BaseURL         string `json:"base_url"`
	APIFormat       string `json:"api_format"`
	MaxTokens       int    `json:"max_tokens"`
	ReasoningEffort string `json:"reasoning_effort"`
	APIKeyEnv       string `json:"api_key_env"`
}

// Tool is one MCP server the agent may call.
type Tool struct {
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers"`
	HeadersEnv string            `json:"headers_env"`
	Timeout    int               `json:"timeout"`
}

// Runtime is what the spec never used to describe: with which model, tools and
// image this agent runs. It lived in cluster manifests, which is why local runs,
// CI and prod could silently drift apart.
type Runtime struct {
	Description string `json:"description"`
	Image       string `json:"image"`
	Model       Model  `json:"model"`
	Tools       []Tool `json:"tools"`
}

// Console names the project and environment this agent is deployed into, so the
// CLI can resolve its tool URLs from the API when the manifest lists none.
type Console struct {
	Project string `json:"project"`
	Env     string `json:"env"`
}

// Langfuse carries the naming contract for evaluation results.
type Langfuse struct {
	DatasetPrefix string `json:"dataset_prefix"`
}

type entry struct {
	Name             string   `json:"name"`
	AgentsRoot       string   `json:"agents_root"`
	Cases            string   `json:"cases"`
	HoldoutThreshold float64  `json:"holdout_threshold"`
	Suites           string   `json:"suites"`
	Judges           string   `json:"judges"`
	Runtime          Runtime  `json:"runtime"`
	Console          Console  `json:"console"`
	Langfuse         Langfuse `json:"langfuse"`
}

type manifest struct {
	Version int     `json:"version"`
	Agents  []entry `json:"agents"`
}

// Spec is one agent's resolved layout: absolute paths plus the declared runtime.
type Spec struct {
	Repo             string
	Name             string
	Dir              string
	Prompt           string
	Domains          string
	Suites           string
	Judges           string
	Cases            string
	HoldoutThreshold float64
	Runtime          Runtime
	Console          Console
	Langfuse         Langfuse
}

// Load reads the manifest in repo and resolves the named agent. The name may be
// empty when the manifest declares exactly one agent.
func Load(repo, name string) (*Spec, error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(abs, ManifestPath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no %s: this repo declares no agent", ManifestPath)
		}
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", ManifestPath, err)
	}
	if m.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported version %d, expected 1", ManifestPath, m.Version)
	}
	if len(m.Agents) == 0 {
		return nil, fmt.Errorf("%s: agents must be a non-empty list", ManifestPath)
	}

	var found *entry
	switch {
	case name != "":
		for i := range m.Agents {
			if m.Agents[i].Name == name {
				found = &m.Agents[i]
			}
		}
		if found == nil {
			return nil, fmt.Errorf("%s: no agent named %q", ManifestPath, name)
		}
	case len(m.Agents) == 1:
		found = &m.Agents[0]
	default:
		names := make([]string, 0, len(m.Agents))
		for _, a := range m.Agents {
			names = append(names, a.Name)
		}
		return nil, fmt.Errorf("pass --agent: the manifest declares %v", names)
	}
	if found.Name == "" {
		return nil, fmt.Errorf("%s: an agent entry has no name", ManifestPath)
	}

	root := found.AgentsRoot
	if root == "" {
		root = "."
	}
	dir := filepath.Join(abs, root, "agents", found.Name)
	resolve := func(rel, fallback string) string {
		if rel == "" {
			return filepath.Join(dir, fallback)
		}
		return filepath.Join(abs, rel)
	}
	spec := &Spec{
		Repo:             abs,
		Name:             found.Name,
		Dir:              dir,
		Prompt:           filepath.Join(dir, "core.md"),
		Domains:          filepath.Join(dir, "domains"),
		Suites:           resolve(found.Suites, filepath.Join("evals", "suites")),
		Judges:           resolve(found.Judges, "judge"),
		HoldoutThreshold: found.HoldoutThreshold,
		Runtime:          found.Runtime,
		Console:          found.Console,
		Langfuse:         found.Langfuse,
	}
	if found.Cases != "" {
		spec.Cases = filepath.Join(abs, found.Cases)
	}
	if _, err := os.Stat(spec.Prompt); err != nil {
		return nil, fmt.Errorf("%s not found: the manifest points at no prompt", spec.Prompt)
	}
	return spec, nil
}

// DatasetPrefix is the Langfuse dataset prefix, defaulting to the agent name.
func (s *Spec) DatasetPrefix() string {
	if s.Langfuse.DatasetPrefix != "" {
		return s.Langfuse.DatasetPrefix
	}
	return s.Name
}

// List returns the files in dir with the given extension, sorted by name.
func List(dir, ext string) []string {
	matches, err := filepath.Glob(filepath.Join(dir, "*"+ext))
	if err != nil {
		return nil
	}
	return matches
}
