package alert

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/dennisme/clickhouse-ruler/internal/rule"
)

// valueKey is how a template reaches the alert's value. Lower case because it
// matches the `value` result column that produced it, so a rule author writes
// the same name in the SQL and in the summary.
const valueKey = "value"

// AnnotationError names an annotation whose template did not render, and why.
//
// One entry per annotation, not per instance: a broken template fails on every
// row a rule returns, and a rule returning ten thousand rows must not report ten
// thousand times (spec 8.3).
type AnnotationError struct {
	Annotation string
	Err        error

	// MissingKeys names the fields the template read that the alert did not
	// carry, sorted, which is what the finding raised on the rule's owner says
	// instead of repeating the Go error (spec 6.5). Empty for a template that
	// never parsed, since it reached no data, and for a failure that was not a
	// missing key at all.
	MissingKeys []string
}

// annotate expands each annotation as a Go template over the alert's labels plus
// its value, and returns what it managed to render.
//
// Annotations are rendered per annotation, and a failure in one never discards
// another. They are independent: an operator's Alertmanager templates read them
// by name, and the one carrying a link to follow is usually not the one with the
// interesting template in it. Discarding the set because a summary named a
// column that is not there would take the useful annotations down with the
// broken one.
//
// A failed annotation carries a short fixed marker, so the page still goes out
// and a human reading it can see which annotation is broken. A missing key is a
// failure rather than an empty string for the same reason: a page reading
// "  p99 is  ms" costs the responder their first minutes, and the failure would
// be invisible until someone is already awake. Prometheus writes the whole error
// into the field instead; the marker is short and the same every time because
// summary is what a PagerDuty title is built from and what automation parses,
// and the error an author needs goes in ErrorAnnotation beside it (spec 6.5).
func annotate(annotations map[string]*template.Template, parseErrs map[string]error, a Alert) (map[string]string, []AnnotationError) {
	if len(annotations) == 0 && len(parseErrs) == 0 {
		return nil, nil
	}

	data := make(map[string]any, len(a.Labels)+1)
	for k, v := range a.Labels {
		data[k] = v
	}
	// Formatted the same way the querier formats a float label, so the number
	// in the summary matches the number in the labels.
	data[valueKey] = strconv.FormatFloat(a.Value, 'f', -1, 64)

	// Sorted so that a rule with two broken annotations reports them in the
	// same order on every evaluation.
	names := make([]string, 0, len(annotations)+len(parseErrs))
	for name := range annotations {
		names = append(names, name)
	}
	for name := range parseErrs {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make(map[string]string, len(names))
	var failures []AnnotationError
	var errors []string

	for _, name := range names {
		err := parseErrs[name]
		if err == nil {
			var rendered string
			rendered, err = execute(name, annotations[name], data)
			if err == nil {
				out[name] = rendered
				continue
			}
		}
		missing := missingKeys(annotations[name], data)
		out[name] = marker(name, missing)
		errors = append(errors, err.Error())
		failures = append(failures, AnnotationError{
			Annotation:  name,
			Err:         err,
			MissingKeys: missing,
		})
	}

	// One field rather than one per failure: a consumer reading the errors has
	// one name to know, and the value is bounded by how many annotations the
	// rule has rather than by how many rows it returned. Absent when everything
	// rendered, so a template testing for it is testing for a real failure.
	if len(errors) > 0 {
		out[rule.ErrorAnnotation] = strings.Join(errors, "; ")
	}
	return out, failures
}

// markerKeys is how many missing labels the marker names before it counts the
// rest. Three, because summary is a notification field: it becomes a PagerDuty
// title and a Slack message, and a template reading a dozen labels must not turn
// one into a list of them.
//
// The cap is on this field alone. MissingKeys carries all of them, and the
// finding the running ruler raises from it names all of them, because a log
// field is read by somebody already debugging that rule (spec 6.5).
const markerKeys = 3

// marker is what a failed annotation carries in place of what it could not
// render.
//
// The prefix is fixed, so a consumer matching on it keeps working and a human
// grepping finds every one. The tail names what the template asked for and the
// alert did not have, because this is the one field a responder is guaranteed to
// read: `ruler_error` beside it carries the whole error and reaches a human only
// where the operator's receiver templates it, which is their file and not ours
// (spec 6.5).
func marker(name string, missing []string) string {
	if len(missing) == 0 {
		return fmt.Sprintf("<ruler: annotation %q failed>", name)
	}

	noun, named := "label", missing
	if len(missing) > 1 {
		noun = "labels"
	}

	var rest string
	if len(named) > markerKeys {
		rest = fmt.Sprintf("%d more", len(named)-markerKeys)
		named = named[:markerKeys]
	}

	quoted := make([]string, 0, len(named)+1)
	for _, key := range named {
		quoted = append(quoted, strconv.Quote(key))
	}
	if rest != "" {
		quoted = append(quoted, rest)
	}

	return fmt.Sprintf("<ruler: annotation %q failed: no %s %s>", name, noun, list(quoted))
}

// list joins names the way a sentence does, so a responder reads the marker
// rather than parsing it.
func list(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}

// missingKeys names the fields a template read that the data does not have.
//
// Read off the parsed template rather than out of the error text, which names
// only the first key execution reached: a summary missing two labels is one
// edit for its author and a finding that named half of it would cost them a
// second evaluation to discover the rest (spec 6.5).
func missingKeys(t *template.Template, data map[string]any) []string {
	var out []string
	for _, field := range rule.TemplateFields(t) {
		if _, ok := data[field]; !ok {
			out = append(out, field)
		}
	}
	return out
}

// parseAnnotations compiles a rule's annotation templates once, because they are
// fixed for the rule's lifetime and a rule returning a thousand rows would
// otherwise re-parse each one a thousand times on every evaluation.
//
// A template that does not parse is kept as its error rather than dropped. The
// annotations/template check reports it at load time (spec 7.3), and at runtime
// it has to reach the page the same way a template that fails to execute does.
func parseAnnotations(annotations map[string]string) (map[string]*template.Template, map[string]error) {
	if len(annotations) == 0 {
		return nil, nil
	}

	parsed := map[string]*template.Template{}
	errs := map[string]error{}

	for name, text := range annotations {
		// Option "missingkey=error" only fires for a map, which is why the data
		// passed to Execute is a map rather than a struct.
		t, err := template.New(name).Option("missingkey=error").Parse(text)
		if err != nil {
			errs[name] = fmt.Errorf("annotation %q: %w", name, err)
			continue
		}
		parsed[name] = t
	}
	return parsed, errs
}

func execute(name string, t *template.Template, data map[string]any) (string, error) {
	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("annotation %q: %w", name, err)
	}
	return sb.String(), nil
}
