package main

import (
	"strings"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/scheduler"
)

// The re-check pass ships on. It is the only thing that catches a renamed OTel
// map key, which is a query that still parses, still returns its columns, still
// succeeds on every tick and matches nothing forever, so a pass that shipped off
// would be a check that does not exist on any ruler nobody configured (spec
// 10.4).
//
// This pins the wiring rather than the constant, because the constant was
// documented as the value used "when an operator asks for it" while the flag
// handed it to every operator who did not.
func TestTheRecheckPassIsOnByDefault(t *testing.T) {
	if scheduler.DefaultRecheckInterval <= 0 {
		t.Fatalf("DefaultRecheckInterval = %s, want a positive interval", scheduler.DefaultRecheckInterval)
	}

	_, stderr := runRunCmd(t, "run", "-h")

	want := "(default " + scheduler.DefaultRecheckInterval.String() + ")"
	line := flagUsage(t, stderr, "-recheck-interval")
	if !strings.Contains(line, want) {
		t.Errorf("--recheck-interval usage does not carry %s, so the pass does not ship on:\n%s", want, line)
	}
}

// flagUsage is the usage block's line for one flag, including the line below it
// where the flag package puts the description and the default.
func flagUsage(t *testing.T, usage, flag string) string {
	t.Helper()

	lines := strings.Split(usage, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), flag) {
			return strings.Join(lines[i:min(i+4, len(lines))], "\n")
		}
	}
	t.Fatalf("%s is not in the usage output:\n%s", flag, usage)
	return ""
}
