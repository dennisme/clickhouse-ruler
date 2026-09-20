package source

import "sort"

// Match returns every source a rule's selector picks out.
//
// A source matches when every term in the selector is present and equal in the
// source's labels, so adding terms narrows the result. That is the ordinary
// selector direction, and the reverse would mean a rule could only ever widen
// its reach by adding labels (spec 6.10).
//
// An empty or nil selector matches nothing. The usual convention is that empty
// matches everything, and it is wrong for this field: selecting a source picks
// the ClickHouse user the query runs as, so omitting the field must not hand a
// rule the whole estate. A rule that should run everywhere selects a label its
// operator put on every source, which says so out loud.
//
// A rule matching several sources evaluates against all of them, so the result
// order reaches alert fingerprints. Sorting by name keeps that independent of
// map iteration and of how the file happened to be written.
func (f *File) Match(selector map[string]string) []Source {
	if f == nil || len(selector) == 0 {
		return nil
	}

	var out []Source
	for _, s := range f.Sources {
		if selects(selector, s) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func selects(selector map[string]string, s Source) bool {
	for k, want := range selector {
		if s.Labels[k] != want {
			return false
		}
	}
	return true
}
