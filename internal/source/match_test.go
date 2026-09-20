package source

import "testing"

func sources() *File {
	return &File{
		File: "sources.yaml",
		Sources: []Source{
			{Name: "payments_main", Labels: map[string]string{"team": "payments", "cluster": "main"}},
			{Name: "payments_prod", Labels: map[string]string{"team": "payments", "cluster": "prod", "env": "prod"}},
			{Name: "search_main", Labels: map[string]string{"team": "search", "cluster": "main"}},
			{Name: "unlabelled"},
		},
	}
}

func names(got []Source) []string {
	out := make([]string, 0, len(got))
	for _, s := range got {
		out = append(out, s.Name)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selector map[string]string
		want     []string
	}{
		{
			// Adding terms narrows. This is the whole reason the selector
			// lives on the rule rather than requirements living on the source.
			name:     "one term selects every source carrying it",
			selector: map[string]string{"team": "payments"},
			want:     []string{"payments_main", "payments_prod"},
		},
		{
			name:     "a second term narrows to one",
			selector: map[string]string{"team": "payments", "env": "prod"},
			want:     []string{"payments_prod"},
		},
		{
			name:     "a label the source lacks excludes it",
			selector: map[string]string{"team": "payments", "env": "staging"},
			want:     nil,
		},
		{
			// A label an operator puts on every source is how a rule says
			// "everywhere" out loud.
			name:     "selecting across teams is possible when a label spans them",
			selector: map[string]string{"cluster": "main"},
			want:     []string{"payments_main", "search_main"},
		},
		{
			// Selecting a source picks the ClickHouse user the query runs as,
			// so omitting the field must not hand a rule the whole estate.
			name:     "empty selector matches nothing",
			selector: map[string]string{},
			want:     nil,
		},
		{
			name:     "nil selector matches nothing",
			selector: nil,
			want:     nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := names(sources().Match(tc.selector)); !equal(got, tc.want) {
				t.Errorf("Match(%v) = %v, want %v", tc.selector, got, tc.want)
			}
		})
	}
}

// The result order reaches fingerprints, so it cannot depend on map iteration
// or on the order sources happen to appear in the file.
func TestMatchIsSortedByName(t *testing.T) {
	f := &File{Sources: []Source{
		{Name: "zulu", Labels: map[string]string{"team": "x"}},
		{Name: "alpha", Labels: map[string]string{"team": "x"}},
		{Name: "mike", Labels: map[string]string{"team": "x"}},
	}}

	for i := 0; i < 20; i++ {
		if got := names(f.Match(map[string]string{"team": "x"})); !equal(got, []string{"alpha", "mike", "zulu"}) {
			t.Fatalf("Match order = %v, want sorted", got)
		}
	}
}
