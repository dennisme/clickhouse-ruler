package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionPrintsDevByDefault(t *testing.T) {
	code, stdout, stderr := runCheck(t, "version")

	if code != 0 {
		t.Errorf("exit = %d, want 0\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "dev") {
		t.Errorf("version should print dev for an unstamped build, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "go1.") {
		t.Errorf("version should print the Go version, got:\n%s", stdout)
	}
}

func TestUsageMentionsVersion(t *testing.T) {
	_, _, stderr := runCheck(t)

	if !strings.Contains(stderr, "version") {
		t.Errorf("usage should mention the version subcommand, got: %s", stderr)
	}
}

// The trap this slice exists to close. `go build -ldflags "-X path.Version=x"`
// against a path that does not exist exits 0 and stamps nothing, so a release
// can ship binaries that silently report no version, and a shipped release
// cannot be re-stamped. Only a real build proves the symbol path resolves.
//
// A real build is also the only place the commit appears: the toolchain
// records vcs.revision for `go build` and not for a test binary, so asserting
// on it from inside `go test` would assert on a field that is always empty.
func TestBuiltBinaryReportsCommitAndStampedVersion(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("no go toolchain: %v", err)
	}
	if out, err := exec.Command("git", "rev-parse", "--git-dir").CombinedOutput(); err != nil {
		t.Skipf("not a git checkout, nothing to stamp a commit from: %s", out)
	}

	build := func(t *testing.T, ldflags string) string {
		t.Helper()

		bin := filepath.Join(t.TempDir(), "ruler")
		args := []string{"build", "-o", bin}
		if ldflags != "" {
			args = append(args, "-ldflags", ldflags)
		}
		args = append(args, ".")

		if out, err := exec.Command("go", args...).CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, out)
		}
		out, err := exec.Command(bin, "version").CombinedOutput()
		if err != nil {
			t.Fatalf("ruler version: %v\n%s", err, out)
		}
		return string(out)
	}

	plain := build(t, "")
	if strings.Contains(plain, "unknown") {
		t.Errorf("a build in a checkout must report a real commit and build time, got:\n%s", plain)
	}
	if !strings.Contains(plain, "dev") {
		t.Errorf("an unstamped build must report dev, got:\n%s", plain)
	}

	stamped := build(t, "-X github.com/dennisme/clickhouse-ruler/internal/buildinfo.Version=v9.9.9")
	if !strings.Contains(stamped, "v9.9.9") {
		t.Errorf("-X did not reach the version symbol, got:\n%s", stamped)
	}
}
