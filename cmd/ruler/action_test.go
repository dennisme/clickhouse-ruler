package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The fixtures the action runs against in this repository's own workflow. A
// green build here says nothing about whether the action works unless the rules
// it checks actually fail (spec 12.7).
func TestActionFixturesFail(t *testing.T) {
	dir := filepath.Join("..", "..", "action", "testdata")

	code, stdout, _ := runCheck(t, "check",
		"--sources", filepath.Join(dir, "sources.yaml"),
		filepath.Join(dir, "rules"))

	if code != exitFinding {
		t.Errorf("exit = %d, want exitFinding: the fixtures are deliberately broken\n%s", code, stdout)
	}
	for _, want := range []string{"rule/expr", "rule/source-match"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("expected a %s finding, got:\n%s", want, stdout)
		}
	}
}

// Nothing is invented for the action that `ruler check` cannot do by hand, so
// anything achievable in CI is achievable on a laptop (spec 10.3). Every input
// is either one of the binary's flags or one of the few things the action owns:
// which release to run, and whether to comment.
func TestActionInputsAreCheckFlags(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "action", "action.yml"))
	if err != nil {
		t.Fatal(err)
	}

	var action struct {
		Inputs map[string]struct {
			Description string `yaml:"description"`
		} `yaml:"inputs"`
	}
	if err := yaml.Unmarshal(data, &action); err != nil {
		t.Fatal(err)
	}
	if len(action.Inputs) == 0 {
		t.Fatal("the action declares no inputs")
	}

	// What the action owns rather than passes on: the wiring 10.3 keeps out of
	// the binary, plus the rules path, which is check's positional argument.
	owned := map[string]bool{
		"version":      true,
		"binary":       true,
		"rules":        true,
		"comment":      true,
		"github-token": true,
	}

	// `check -h` prints its own flags, so the binary is the authority here
	// rather than a list this test repeats.
	_, _, usage := runCheck(t, "check", "-h")

	for name, input := range action.Inputs {
		if input.Description == "" {
			t.Errorf("input %s has no description", name)
		}
		if owned[name] {
			continue
		}
		if !strings.Contains(usage, "-"+name+" ") && !strings.Contains(usage, "-"+name+"\n") {
			t.Errorf("input %s is not a `ruler check` flag:\n%s", name, usage)
		}
	}
}

// The action's inputs are documented on the site rather than in `action/`
// (spec 10.3), which puts the list in a file the wiring does not compile. An
// input renamed in `action.yml` and left alone on the page is a workflow
// somebody writes from the documentation and cannot run, so the two are
// compared here the way `just generate-check` compares the check pages.
func TestActionInputsAreDocumented(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "action", "action.yml"))
	if err != nil {
		t.Fatal(err)
	}

	var action struct {
		Inputs map[string]struct{} `yaml:"inputs"`
	}
	if err := yaml.Unmarshal(data, &action); err != nil {
		t.Fatal(err)
	}
	if len(action.Inputs) == 0 {
		t.Fatal("the action declares no inputs")
	}

	page, err := os.ReadFile(filepath.Join("..", "..", "docs", "pull-requests.md"))
	if err != nil {
		t.Fatal(err)
	}
	documented := documentedInputs(t, string(page))

	for name := range action.Inputs {
		if !documented[name] {
			t.Errorf("input %s is not on the page", name)
		}
	}
	for name := range documented {
		if _, ok := action.Inputs[name]; !ok {
			t.Errorf("the page documents %s, which the action does not declare", name)
		}
	}
}

// documentedInputs reads the first column of the page's input table, which is
// the one table whose rows are named after something in action.yml.
func documentedInputs(t *testing.T, page string) map[string]bool {
	t.Helper()

	const heading = "## Inputs"
	start := strings.Index(page, heading)
	if start < 0 {
		t.Fatalf("the page has no %q section", heading)
	}
	table := page[start+len(heading):]
	if end := strings.Index(table, "\n## "); end >= 0 {
		table = table[:end]
	}

	out := map[string]bool{}
	for _, line := range strings.Split(table, "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cell := strings.TrimPrefix(line, "| `")
		name, _, ok := strings.Cut(cell, "`")
		if !ok {
			t.Errorf("unclosed input name in table row: %s", line)
			continue
		}
		out[name] = true
	}
	if len(out) == 0 {
		t.Fatalf("no input rows found under %q", heading)
	}
	return out
}
