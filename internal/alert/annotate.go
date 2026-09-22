package alert

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/template"
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
// A failed annotation carries its failure as its value, so the page still goes
// out and a human reading it can see which annotation is broken. A missing key
// is a failure rather than an empty string for the same reason: a page reading
// "  p99 is  ms" costs the responder their first minutes, and the failure would
// be invisible until someone is already awake. Prometheus does the same, on the
// grounds that a ruler which drops a page over a bad summary is worse than one
// that pages with a bad summary.
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
		out[name] = fmt.Sprintf("<error expanding template: %s>", err)
		failures = append(failures, AnnotationError{Annotation: name, Err: err})
	}
	return out, failures
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
