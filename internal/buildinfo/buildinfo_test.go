package buildinfo

import (
	"runtime/debug"
	"strings"
	"testing"
)

// The release pipeline stamps Version with -X and nothing else, so an
// unstamped build has to say so rather than print an empty field that reads
// like a version.
func TestVersionDefaultsToDev(t *testing.T) {
	if Version != "dev" {
		t.Errorf("Version = %q, want dev for an unstamped build", Version)
	}
}

func TestReadTakesCommitAndTimeFromVCS(t *testing.T) {
	got := read(&debug.BuildInfo{
		GoVersion: "go1.26.8",
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "a3995179c414607f0a9474d47697484dfa0b0382"},
			{Key: "vcs.time", Value: "2026-09-27T04:19:18Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	})

	if got.Commit != "a3995179c414607f0a9474d47697484dfa0b0382" {
		t.Errorf("Commit = %q", got.Commit)
	}
	if got.BuildTime != "2026-09-27T04:19:18Z" {
		t.Errorf("BuildTime = %q", got.BuildTime)
	}
	if !got.Dirty {
		t.Error("Dirty = false, want true from vcs.modified")
	}
	if got.GoVersion != "go1.26.8" {
		t.Errorf("GoVersion = %q", got.GoVersion)
	}
	if got.Version != Version {
		t.Errorf("Version = %q, want %q", got.Version, Version)
	}
}

// A binary built outside a checkout, and every test binary, carries no vcs
// settings at all. The fields still have to print as something a person can
// read back in a bug report.
func TestReadWithoutVCSSettings(t *testing.T) {
	got := read(&debug.BuildInfo{GoVersion: "go1.26.8"})

	if got.Commit != "unknown" {
		t.Errorf("Commit = %q, want unknown", got.Commit)
	}
	if got.BuildTime != "unknown" {
		t.Errorf("BuildTime = %q, want unknown", got.BuildTime)
	}
	if got.Dirty {
		t.Error("Dirty = true, want false when vcs.modified is absent")
	}
}

func TestStringNamesEveryField(t *testing.T) {
	s := Info{
		Version:   "v1.2.3",
		Commit:    "abc123",
		BuildTime: "2026-09-27T04:19:18Z",
		Dirty:     true,
		GoVersion: "go1.26.8",
	}.String()

	for _, want := range []string{"v1.2.3", "abc123", "2026-09-27T04:19:18Z", "true", "go1.26.8"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() missing %q:\n%s", want, s)
		}
	}
}
