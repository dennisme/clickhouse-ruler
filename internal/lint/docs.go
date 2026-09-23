package lint

import "strings"

// DocsBase is where the check pages are published.
//
// A constant rather than a flag. A link that only resolves when an operator
// remembered to pass something is a link that usually does not resolve, and a
// finding naming a check nobody can look up is the thing spec 7.8 exists to
// prevent.
const DocsBase = "https://dennisme.github.io/clickhouse-ruler/checks/"

// pages maps a check's namespace to the page it is documented on.
//
// Grouped rather than one page per check. Most of what a page has to say is
// the same for every check in a namespace, and repeating it 38 times produces
// documentation nobody reads and nobody keeps current.
var pages = map[string]string{
	"rule":        "rule.md",
	"labels":      "rule.md",
	"annotations": "rule.md",
	"source":      "source.md",
	"policy":      "policy.md",
	"yaml":        "policy.md",
	"ruleset":     "policy.md",
}

// DocsPage returns the file a check is documented in, empty for a name the
// table does not know.
func DocsPage(check string) string {
	if !Known(check) {
		return ""
	}
	namespace, _, _ := strings.Cut(check, "/")
	return pages[namespace]
}

// DocsAnchor returns a check's heading anchor.
//
// Written into the page rather than inferred from the heading text, because
// GitHub's slugifier drops the slash rather than replacing it: the heading
// `rule/foreign-table` would otherwise be reachable only as
// `ruleforeign-table`. Both the generator and the links here call this, so
// the convention lives in one place instead of two that agree today.
func DocsAnchor(check string) string {
	return strings.ReplaceAll(check, "/", "-")
}

// DocsURL returns where a check is explained, empty for a name the table does
// not know.
//
// The published path rather than the source file: the site serves rule.md at
// checks/rule/, so a URL carrying the .md would 404 on every finding.
func DocsURL(check string) string {
	page := DocsPage(check)
	if page == "" {
		return ""
	}
	return DocsBase + strings.TrimSuffix(page, ".md") + "/#" + DocsAnchor(check)
}
