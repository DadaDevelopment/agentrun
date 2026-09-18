// Package agentspec finds the agent a repository contains.
//
// There is no manifest to write. An agent repo is recognised by its layout -
// `agents/<name>/core.md` is the prompt, `agents/<name>/domains/*.md` are the
// skills - and everything else has a default or a flag. A repo that follows
// the layout works with ddc the moment it is cloned.
//
// Where the agent is deployed is not a property of the source tree either: it
// is remembered per working directory in the user's own config, exactly like
// `ddc deploy` remembers an app's project. Two people can run the same repo
// against different environments without editing a committed file.
package agentspec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// OverridePath is the optional file for settings that genuinely belong to the
// repo rather than to a person: a pinned image, non-default tools. Most repos
// do not have one.
const OverridePath = ".dada/agent.json"

// Model is how to call the LLM. Only the name of the key variable is stored.
type Model struct {
	Name            string `json:"name,omitempty"`
	BaseURL         string `json:"base_url,omitempty"`
	APIFormat       string `json:"api_format,omitempty"`
	MaxTokens       int    `json:"max_tokens,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	APIKeyEnv       string `json:"api_key_env,omitempty"`
}

// Tool is one MCP server the agent may call.
type Tool struct {
	Name       string            `json:"name,omitempty"`
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers,omitempty"`
	HeadersEnv string            `json:"headers_env,omitempty"`
	Timeout    int               `json:"timeout,omitempty"`
}

// Runtime is what the agent runs with. Empty fields fall back to ddc's
// defaults, so a repo declares only what differs.
type Runtime struct {
	Description string `json:"description,omitempty"`
	Image       string `json:"image,omitempty"`
	Model       Model  `json:"model,omitempty"`
	Tools       []Tool `json:"tools,omitempty"`
}

type override struct {
	Agents map[string]Runtime `json:"agents"`
}

// Spec is one agent as found in a repository.
type Spec struct {
	Repo    string
	Name    string
	Dir     string
	Prompt  string
	Domains string
	Suites  string
	Judges  string
	Runtime Runtime
}

// Discover finds the agent in repo. The name may be empty when the repo holds
// exactly one agent, which is the usual case.
func Discover(repo, name string) (*Spec, error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	found, err := agentNames(abs)
	if err != nil {
		return nil, err
	}
	switch {
	case len(found) == 0:
		return nil, fmt.Errorf("no agent in %s: expected a prompt at agents/<name>/core.md", abs)
	case name == "" && len(found) > 1:
		return nil, fmt.Errorf("pass --agent: this repo holds %s", strings.Join(found, ", "))
	case name == "":
		name = found[0]
	default:
		if !contains(found, name) {
			return nil, fmt.Errorf("no agent %q in %s (found: %s)", name, abs, strings.Join(found, ", "))
		}
	}

	dir := filepath.Join(abs, "agents", name)
	spec := &Spec{
		Repo:    abs,
		Name:    name,
		Dir:     dir,
		Prompt:  filepath.Join(dir, "core.md"),
		Domains: filepath.Join(dir, "domains"),
		Suites:  filepath.Join(dir, "evals", "suites"),
		Judges:  filepath.Join(dir, "judge"),
	}
	runtime, err := loadOverride(abs, name)
	if err != nil {
		return nil, err
	}
	spec.Runtime = runtime
	return spec, nil
}

// agentNames lists the directories under agents/ that hold a prompt.
func agentNames(repo string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(repo, "agents"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(repo, "agents", entry.Name(), "core.md")); err == nil {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// loadOverride reads the optional per-repo runtime settings. A missing file is
// the normal case, not an error.
func loadOverride(repo, name string) (Runtime, error) {
	raw, err := os.ReadFile(filepath.Join(repo, OverridePath))
	if os.IsNotExist(err) {
		return Runtime{}, nil
	}
	if err != nil {
		return Runtime{}, err
	}
	var file override
	if err := json.Unmarshal(raw, &file); err != nil {
		return Runtime{}, fmt.Errorf("%s: %w", OverridePath, err)
	}
	return file.Agents[name], nil
}

// List returns the files in dir with the given extension, sorted by name.
func List(dir, ext string) []string {
	matches, err := filepath.Glob(filepath.Join(dir, "*"+ext))
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	return matches
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
