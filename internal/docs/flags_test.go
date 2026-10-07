package docs

import (
	"flag"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/cli"
)

// page is the checked-in flag reference, read from the test so that what is
// asserted is what a reader will actually open.
const page = "../../docs/running.md"

func readFlagsPage(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(page)
	if err != nil {
		t.Fatalf("reading %s: %v", page, err)
	}
	return string(data)
}

// rows reads the flag names out of the generated region, which is the only
// part of the page the generator owns. A flag named in the prose outside the
// markers is a flag explained rather than listed, and this table is the list.
func rows(t *testing.T, content string) map[string]bool {
	t.Helper()

	region, err := Region(content)
	if err != nil {
		t.Fatal(err)
	}

	out := map[string]bool{}
	for _, m := range regexp.MustCompile("`--([a-z-]+)`").FindAllStringSubmatch(region, -1) {
		out[m[1]] = true
	}
	return out
}

// Both directions, the way the check pages are asserted. A flag missing from
// the table is a flag an operator cannot discover; a row naming a flag the
// binary does not take sends them to type something that fails.
func TestEveryFlagIsListedAndEveryRowIsAFlag(t *testing.T) {
	listed := rows(t, readFlagsPage(t))

	registered := map[string]bool{}
	for _, c := range cli.Commands() {
		c.Flags.VisitAll(func(f *flag.Flag) {
			registered[f.Name] = true
			if !listed[f.Name] {
				t.Errorf("ruler %s --%s is not in the flag table, so nothing tells a reader it exists", c.Name, f.Name)
			}
		})
	}

	for name := range listed {
		if !registered[name] {
			t.Errorf("--%s is in the flag table and no command registers it", name)
		}
	}
}

// A row with no usage text passes a completeness test and tells a reader
// nothing, which is the same shortcut TestEverySectionSaysSomething closes on
// the check pages.
func TestEveryFlagSaysWhatItDoes(t *testing.T) {
	for _, c := range cli.Commands() {
		c.Flags.VisitAll(func(f *flag.Flag) {
			if strings.TrimSpace(f.Usage) == "" {
				t.Errorf("ruler %s --%s has no usage string, so the table has nothing to print", c.Name, f.Name)
			}
		})
	}
}

// The table is generated, so the checked-in page has to be what the flag set
// produces. `just check` runs the generator and diffs the tree, and this is
// the same gate as a unit test for anyone running one and not the other.
func TestTheCheckedInTableIsWhatTheFlagSetProduces(t *testing.T) {
	content := readFlagsPage(t)

	updated, err := Apply("running.md", content)
	if err != nil {
		t.Fatal(err)
	}
	if updated != content {
		t.Error("docs/running.md is not what the flag set renders, run `just generate`")
	}
}

// Every command the binary dispatches on has to be in the reference, or the
// page documents one subcommand's flags and silently omits another's.
func TestEveryCommandWithFlagsIsCovered(t *testing.T) {
	var names []string
	for _, c := range cli.Commands() {
		names = append(names, c.Name)
	}

	for _, want := range []string{"check", "run"} {
		found := false
		for _, got := range names {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("ruler %s registers flags and is not in the reference", want)
		}
	}
}
