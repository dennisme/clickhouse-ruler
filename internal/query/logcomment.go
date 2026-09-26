package query

import (
	"encoding/json"
	"maps"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// logCommentSetting is what ClickHouse stores per query and writes into the
// log_comment column of system.query_log.
const logCommentSetting = "log_comment"

// rulerName marks a query as this ruler's, so an operator can select every
// one of them before they know which rule they are looking for.
const rulerName = "clickhouse-ruler"

// logComment says which rule sent a query, for reading back in
// system.query_log (spec 8.5).
//
// The other half of operating this thing happens on the cluster: a rule that
// times out, reads more than the estimate predicted or trips a memory cap
// leaves its evidence there rather than in anything the ruler exposes. Until
// now the only way to pick those rows out was the source's username, which
// names a source and therefore cannot separate one rule from another, and the
// ruler-per-team and ruler-as-a-service topologies in 10.2 deliberately share
// a user across many rules.
//
// It carries the rule and its group and nothing else. The query text is
// already in the log, and label values are data: putting them here would put
// unbounded cardinality into a column operators group by (spec 8.3).
//
// JSON, so the column is read with JSONExtractString rather than parsed by
// hand, and so a name carrying a quote cannot change the shape of what is
// stored.
func logComment(group, rule string) string {
	// Marshalling three strings has no failure mode.
	b, _ := json.Marshal(struct {
		Ruler string `json:"ruler"`
		Group string `json:"rule_group"`
		Rule  string `json:"rule"`
	}{Ruler: rulerName, Group: group, Rule: rule})

	return string(b)
}

// withLogComment returns the caller's settings with the comment added.
//
// A copy rather than a write into the argument, because clickhouse.Settings
// replaces rather than merges: WithSettings takes the whole map, so the
// comment has to travel with the caps in spec 6.7 instead of over them.
func withLogComment(s clickhouse.Settings, group, rule string) clickhouse.Settings {
	out := make(clickhouse.Settings, len(s)+1)
	maps.Copy(out, s)
	out[logCommentSetting] = logComment(group, rule)
	return out
}
