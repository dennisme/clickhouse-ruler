package query

import (
	"fmt"
	"sort"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// shapeOf pairs what the driver reported about a result: the column names and
// the database type of each, which is what the comparison in 6.3.2 reads.
func shapeOf(names []string, types []driver.ColumnType) []Column {
	out := make([]Column, 0, len(names))
	for i, name := range names {
		var dbType string
		if i < len(types) {
			dbType = types[i].DatabaseTypeName()
		}
		out = append(out, Column{Name: name, Type: dbType})
	}
	return out
}

// Differences describes every way two results fail to be the same result,
// naming each side the way the caller names it: two sources at check time, and
// the previous evaluation against this one while the ruler is running
// (spec 6.10, 6.3.2).
//
// Only the names and the types are compared. Column order decides nothing: an
// alert's labels are keyed by name (spec 6.3), so two results carrying the same
// columns in a different order produce the same alerts.
//
// One line per column that differs, in name order, so the same difference is
// always described the same way.
func Differences(a, b []Column, aName, bName string) []string {
	aTypes, bTypes := typesByName(a), typesByName(b)

	var out []string
	for _, name := range columnNames(aTypes, bTypes) {
		aType, inA := aTypes[name]
		bType, inB := bTypes[name]

		switch {
		case inA && inB && aType != bType:
			out = append(out, fmt.Sprintf("%s returns %s as %s and %s returns it as %s",
				aName, name, aType, bName, bType))
		case inA && !inB:
			out = append(out, fmt.Sprintf("%s returns %s and %s does not", aName, name, bName))
		case inB && !inA:
			out = append(out, fmt.Sprintf("%s returns %s and %s does not", bName, name, aName))
		}
	}
	return out
}

// typesByName indexes a result by column name, which is the key everything
// downstream reads it by.
func typesByName(cols []Column) map[string]string {
	out := make(map[string]string, len(cols))
	for _, c := range cols {
		out[c.Name] = c.Type
	}
	return out
}

// columnNames is every column name either result carries, sorted, so a finding
// lists them the same way on every run.
func columnNames(a, b map[string]string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	for name := range a {
		seen[name] = true
	}
	for name := range b {
		seen[name] = true
	}

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
