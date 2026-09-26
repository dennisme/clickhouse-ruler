// Package dashboards reads the Grafana dashboards this repository ships, so
// what they query can be checked against what the ruler exposes.
//
// A panel querying a metric nobody exposes renders an empty graph, which
// looks exactly like a healthy system. That is the failure this package
// exists to catch, and it is the same shape as a check page documenting a
// default the tool does not have (spec 8.6, 7.8).
package dashboards

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Dir is the checked-in dashboards, read from the tests so that what is
// asserted is what Grafana will import.
const Dir = "../../deploy/grafana/dashboards"

// Dashboard is the part of the Grafana model this repository asserts on.
// Everything else in the file is layout.
type Dashboard struct {
	Title      string `json:"title"`
	Templating struct {
		List []Variable `json:"list"`
	} `json:"templating"`
	Panels []Panel `json:"panels"`
}

// Variable is one entry in the dashboard's template list.
type Variable struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Panel is one graph, stat or table. A row carries its own panels, so the
// shape is recursive.
type Panel struct {
	Title   string   `json:"title"`
	Targets []Target `json:"targets"`
	Panels  []Panel  `json:"panels"`
}

// Target is one query behind a panel.
type Target struct {
	Expr string `json:"expr"`
}

// Load reads one dashboard by file name, from the directory they ship in.
func Load(name string) (Dashboard, error) {
	data, err := Read(name)
	if err != nil {
		return Dashboard{}, err
	}

	var d Dashboard
	if err := json.Unmarshal(data, &d); err != nil {
		return Dashboard{}, fmt.Errorf("parsing %s: %w", name, err)
	}
	return d, nil
}

// Read is one dashboard file as it is checked in. Rooted at the directory
// rather than taking a path, so a name is a name and nothing here opens a
// file somewhere else.
func Read(name string) ([]byte, error) {
	return fs.ReadFile(os.DirFS(Dir), name)
}

// AllPanels flattens rows, keeping the order the file has them in: a
// dashboard leads on what it is for, and the first panel is the assertion.
func (d Dashboard) AllPanels() []Panel {
	var out []Panel
	for _, p := range d.Panels {
		out = append(out, p)
		out = append(out, p.Panels...)
	}
	return out
}

// Expressions is every query in the dashboard.
func (d Dashboard) Expressions() []string {
	var out []string
	for _, p := range d.AllPanels() {
		for _, t := range p.Targets {
			if t.Expr != "" {
				out = append(out, t.Expr)
			}
		}
	}
	return out
}

// HasVariable reports whether the dashboard declares a template variable by
// that name.
func (d Dashboard) HasVariable(name string) bool {
	for _, v := range d.Templating.List {
		if v.Name == name {
			return true
		}
	}
	return false
}

var (
	// A label matcher holds label names and author-supplied values, neither
	// of which is a metric.
	labelMatchers = regexp.MustCompile(`\{[^}]*\}`)

	// A range selector holds a duration, not a series.
	ranges = regexp.MustCompile(`\[[^\]]*\]`)

	// The label list an aggregation groups by names labels, not series.
	grouping = regexp.MustCompile(`\b(by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)`)

	// A Grafana variable is $name or ${name}, and the name that follows is
	// not a metric either.
	variables = regexp.MustCompile(`\$\{?\w+\}?`)

	identifiers = regexp.MustCompile(`[a-zA-Z_][a-zA-Z0-9_]*\s*\(?`)
)

// promQLKeywords are the words in an expression that are the language rather
// than a series. Functions are excluded by the parenthesis that follows them,
// so only the bare words need naming here.
var promQLKeywords = map[string]bool{
	"by": true, "without": true, "on": true, "ignoring": true,
	"group_left": true, "group_right": true, "offset": true, "bool": true,
	"and": true, "or": true, "unless": true, "le": true, "inf": true,
	"nan": true, "start": true, "end": true, "atan2": true,
	// Aggregations, which are written with the grouping clause between the
	// operator and its parenthesis: `sum by (rule_group) (...)`.
	"sum": true, "min": true, "max": true, "avg": true, "count": true,
	"group": true, "stddev": true, "stdvar": true, "topk": true,
	"bottomk": true, "quantile": true, "count_values": true,
}

// histogramSuffixes are what client_golang appends to a histogram's own
// name. A panel reads the buckets; the registry knows the histogram.
var histogramSuffixes = []string{"_bucket", "_sum", "_count"}

// MetricNames is every series an expression reads, sorted and deduplicated.
//
// Read with a regular expression rather than a PromQL parser, because the
// alternative is a dependency for one assertion, and adding one is a
// decision to raise rather than to make. The cost is that this is a reader
// of the shape the dashboards are written in: label matchers, variables and
// function calls are removed, and whatever bare word is left is a metric.
func MetricNames(expr string) []string {
	cleaned := labelMatchers.ReplaceAllString(expr, " ")
	cleaned = ranges.ReplaceAllString(cleaned, " ")
	cleaned = variables.ReplaceAllString(cleaned, " ")
	cleaned = grouping.ReplaceAllString(cleaned, " ")

	seen := map[string]bool{}
	for _, match := range identifiers.FindAllString(cleaned, -1) {
		if strings.HasSuffix(match, "(") {
			// A function call, not a series.
			continue
		}
		name := strings.TrimSpace(match)
		if promQLKeywords[name] {
			continue
		}
		seen[baseName(name)] = true
	}

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// baseName strips the suffix a histogram's exposed series carry, so a panel
// reading _bucket is checked against the histogram that produces it.
func baseName(name string) string {
	for _, suffix := range histogramSuffixes {
		if trimmed, ok := strings.CutSuffix(name, suffix); ok {
			return trimmed
		}
	}
	return name
}

// DatasourceUIDs is every datasource a dashboard file points at. A UID that
// is not a variable is the one from the Grafana it was exported from, and
// exists nowhere else (spec 8.6).
//
// Read from the whole document rather than from the panels, because a
// datasource reference can sit on a panel, a target or a template variable,
// and one hardcoded anywhere is one an import cannot resolve.
func DatasourceUIDs(raw []byte) []string {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}

	var out []string
	var walk func(node any)
	walk = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			for key, value := range n {
				if key == "datasource" {
					if ds, ok := value.(map[string]any); ok {
						if uid, ok := ds["uid"].(string); ok {
							out = append(out, uid)
						}
					}
				}
				walk(value)
			}
		case []any:
			for _, value := range n {
				walk(value)
			}
		}
	}
	walk(doc)

	sort.Strings(out)
	return out
}
