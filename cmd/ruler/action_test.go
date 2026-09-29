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
