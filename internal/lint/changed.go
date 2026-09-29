package lint

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Filter decides which findings a pull request is answerable for.
//
// A zero Filter keeps everything, which is what `ruler check` with no
// --changed-since does: the whole directory, because that is what the loader
// reads at startup, and a CI run whose scope quietly differs from the
// loader's is a rule that passes review and fails to load (spec 10.3).
type Filter struct {
	// files is the changed set, already resolved to absolute paths. A nil map
	// keeps everything, so the failure modes below cannot narrow anything by
	// accident.
	files map[string]bool

	// Note is why every finding is being reported, for a caller to print.
	// Empty when the filter is narrowing. Never empty when it widened, because
	// a run that silently checked more than asked is as confusing as one that
	// silently checked less.
	Note string
}

// ChangedSince builds a filter for the rules a branch touched, comparing the
// work tree against its merge base with ref.
//
// dir is anywhere inside the repository, normally the rules directory. widen
// holds paths whose change affects rules the diff does not name: the sources
// file and the policy file the caller was given. Any ruler.yaml in the changed
// set widens too, since a team file raises checks for the tree below it and is
// not named on the command line (spec 7.7).
//
// Never returns an error. Every way this can fail is a reason to check more
// rather than fewer rules, so a failure widens and says so: filtering that
// silently checks nothing is the failure mode that makes a green build
// meaningless (spec 10.3).
func ChangedSince(dir, ref string, widen []string) Filter {
	base, err := git(dir, "merge-base", "HEAD", ref)
	if err != nil {
		return Filter{Note: fmt.Sprintf(
			"checking every rule: no merge base with %s, which a shallow checkout also causes (%v)", ref, err)}
	}

	// Against the work tree rather than against HEAD, so a rule edited and not
	// yet committed is in scope on a laptop.
	diff, err := git(dir, "diff", "--name-only", base)
	if err != nil {
		return Filter{Note: fmt.Sprintf("checking every rule: cannot read the diff against %s (%v)", base, err)}
	}
	// A rule written and not yet added is exactly the rule its author wants
	// checked, and the diff does not carry it.
	untracked, err := git(dir, "ls-files", "--others", "--exclude-standard", "--full-name")
	if err != nil {
		return Filter{Note: fmt.Sprintf("checking every rule: cannot list untracked files (%v)", err)}
	}

	// git prints repository-relative paths, and a finding carries the path the
	// loader walked, so both sides are resolved to compare.
	root, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return Filter{Note: fmt.Sprintf("checking every rule: cannot find the repository root (%v)", err)}
	}

	widened := make(map[string]bool, len(widen))
	for _, path := range widen {
		// A caller passes the flags it was given, and an unset flag is empty.
		if path == "" {
			continue
		}
		widened[resolvePath(path)] = true
	}

	files := make(map[string]bool)
	for _, rel := range append(lines(diff), lines(untracked)...) {
		path := resolvePath(filepath.Join(root, rel))

		if widened[path] || filepath.Base(rel) == "ruler.yaml" {
			return Filter{Note: fmt.Sprintf(
				"checking every rule: %s changed, which affects rules the diff does not name", rel)}
		}
		files[path] = true
	}
	return Filter{files: files}
}

// Keep returns the findings that belong to the changed files.
//
// Narrowing the findings rather than the reading. The loader still walks the
// whole tree, because the binding and the duplicate checks are answers about
// the tree rather than about one file, so this can only drop a finding a full
// run would also have reported (spec 10.3).
func (f Filter) Keep(problems []Problem) []Problem {
	if f.files == nil {
		return problems
	}

	kept := make([]Problem, 0, len(problems))
	for _, p := range problems {
		if f.files[resolvePath(p.File)] {
			kept = append(kept, p)
		}
	}
	return kept
}

// git runs one read-only command in dir and returns its trimmed output.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // a ref an operator passed
	out, err := cmd.Output()
	if err != nil {
		// The message git wrote is the useful half, and Output discards it
		// into the error rather than into the note a caller prints.
		var stderr string
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			stderr = strings.TrimSpace(string(exit.Stderr))
		}
		if stderr != "" {
			return "", fmt.Errorf("git %s: %s", args[0], stderr)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

func lines(out string) []string {
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// resolvePath makes a path comparable regardless of how it was spelled: the
// loader walks what an operator typed, git prints what the index holds, and on
// a macOS temporary directory one of them goes through a symlink.
func resolvePath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}
