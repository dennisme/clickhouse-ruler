package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

// dir is the checked-in documentation, read from the tests so that what is
// asserted is what a reader will actually open.
const dir = "../../docs/checks"

func readPage(t *testing.T, page string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, page))
	if err != nil {
		t.Fatalf("reading %s: %v", page, err)
	}
	return string(data)
}

// Both directions. A check with no section ships undocumented; a section with
// no check documents something that cannot be reported, which is worse,
// because a reader has no way to tell that it is stale (spec 7.8).
func TestEveryCheckIsDocumentedAndEverySectionIsACheck(t *testing.T) {
	documented, err := CheckedIn(func(page string) (string, error) {
		data, err := os.ReadFile(filepath.Join(dir, page))
		return string(data), err
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range lint.All() {
		if _, ok := documented[c.Name]; !ok {
			t.Errorf("%s has no section, so a finding naming it leads nowhere", c.Name)
		}
	}

	for name := range documented {
		if !lint.Known(name) {
			t.Errorf("%s is documented and is not a check the ruler can report", name)
		}
	}
}

// A heading with nothing under it satisfies a completeness test and helps
// nobody, which makes it the shortcut worth closing.
func TestEverySectionSaysSomething(t *testing.T) {
	documented, err := CheckedIn(func(page string) (string, error) {
		data, err := os.ReadFile(filepath.Join(dir, page))
		return string(data), err
	})
	if err != nil {
		t.Fatal(err)
	}

	for name, body := range documented {
		if len(body) < 80 {
			t.Errorf("%s has %d characters under its heading, which is not an explanation",
				name, len(body))
		}
	}
}

// Every check lands on the page its namespace maps to, or its link is a 404
// even though the section exists.
func TestSectionsAreOnTheirOwnPage(t *testing.T) {
	for _, page := range Pages {
		for name := range Headings(readPage(t, page)) {
			if got := lint.DocsPage(name); got != page {
				t.Errorf("%s is documented in %s, but its link points at %s", name, page, got)
			}
		}
	}
}

// The anchor is what a finding's URL ends with, so the heading has to carry
// exactly the one the link builder produces.
func TestHeadingsCarryTheLinkedAnchor(t *testing.T) {
	for _, page := range Pages {
		content := readPage(t, page)

		for _, c := range lint.All() {
			if lint.DocsPage(c.Name) != page {
				continue
			}
			want := HeadingFor(c.Name)
			if !strings.Contains(content, want) {
				t.Errorf("%s does not carry the heading %q that %s points at",
					page, want, lint.DocsURL(c.Name))
			}
		}
	}
}

// The same guarantee `just generate` gives CI, asserted here as well: a
// contributor who edits the table and runs the tests fails immediately rather
// than at review time.
func TestGeneratedRegionsAreCurrent(t *testing.T) {
	for _, page := range Pages {
		content := readPage(t, page)

		want, err := Apply(page, content)
		if err != nil {
			t.Fatalf("%s: %v", page, err)
		}
		if content != want {
			t.Errorf("%s is out of date, run `just generate`", page)
		}
	}
}

func TestIndexIsCurrent(t *testing.T) {
	if got := readPage(t, "index.md"); got != Index() {
		t.Error("index.md is out of date, run `just generate`")
	}
}

// Apply refuses a page whose markers are gone rather than appending a second
// table to it on every run.
func TestApplyNeedsItsMarkers(t *testing.T) {
	if _, err := Apply("rule.md", "# Rule checks\n\nno markers here\n"); err == nil {
		t.Error("Apply accepted a page with no generated region")
	}
	if _, err := Apply("rule.md", markerEnd+"\n"+markerStart+"\n"); err == nil {
		t.Error("Apply accepted a page whose markers are inverted")
	}
}

// The prose outside the markers survives generation. That is the whole
// arrangement: facts are generated, explanations are written.
func TestApplyKeepsTheProse(t *testing.T) {
	page := "# Rule checks\n\nprose above\n\n" + markerStart + "\nstale\n" + markerEnd +
		"\n\n" + HeadingFor("rule/expr") + "\n\nprose below\n"

	got, err := Apply("rule.md", page)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, want := range []string{"prose above", "prose below", HeadingFor("rule/expr")} {
		if !strings.Contains(got, want) {
			t.Errorf("generation dropped %q", want)
		}
	}
	if strings.Contains(got, "stale") {
		t.Error("generation kept the old table")
	}
}

// The reverse direction has to be able to fail. A section headed with a name
// the table does not know is read back as a heading, so the test that reports
// it has something to find.
func TestHeadingsReadsASectionThatIsNotACheck(t *testing.T) {
	got := Headings("<a id=\"rule-invented\"></a>\n### rule/invented\n\nstale prose.\n")

	if _, ok := got["rule/invented"]; !ok {
		t.Fatal("a section naming an unknown check was skipped, so nothing could report it")
	}
	if lint.Known("rule/invented") {
		t.Fatal("this test needs a name the table does not have")
	}
}

func TestHeadingsReadsBodies(t *testing.T) {
	got := Headings(HeadingFor("rule/expr") + "\n\nwhat it rejects.\n\n" +
		HeadingFor("rule/for") + "\n\nthe other one.\n")

	if len(got) != 2 {
		t.Fatalf("got %d headings, want 2: %v", len(got), got)
	}
	if got["rule/expr"] != "what it rejects." {
		t.Errorf("body = %q, want the prose under the heading", got["rule/expr"])
	}
}

// The anchor is an HTML element rather than the `{#id}` attribute list
// MkDocs understands, because GitHub Flavored Markdown has neither: the
// braces would render as text in the heading and the link would scroll
// nowhere. An <a id> renders on GitHub today and still works if the pages are
// ever published through a site generator.
func TestHeadingFormatIsPortable(t *testing.T) {
	got := HeadingFor("rule/foreign-table")

	if !strings.Contains(got, `<a id="rule-foreign-table"></a>`) {
		t.Errorf("heading = %q, want an HTML anchor GitHub honours", got)
	}
	if strings.Contains(got, "{#") {
		t.Errorf("heading = %q, want no attribute list: GitHub renders it as text", got)
	}
	if !strings.Contains(got, "### rule/foreign-table") {
		t.Errorf("heading = %q, want the check's name as the heading text", got)
	}
}

// The anchor line belongs to the heading, not to the section above it.
func TestHeadingsDoNotKeepTheAnchorInTheBodyAbove(t *testing.T) {
	got := Headings(HeadingFor("rule/expr") + "\n\nfirst.\n\n" + HeadingFor("rule/for") + "\n\nsecond.\n")

	if got["rule/expr"] != "first." {
		t.Errorf("body = %q, want the prose alone", got["rule/expr"])
	}
	if got["rule/for"] != "second." {
		t.Errorf("body = %q, want the prose alone", got["rule/for"])
	}
}
