package query

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// tableRef is a database and a table name, together because the two questions
// below are always asked about a pair.
type tableRef struct {
	Database string
	Table    string
}

// storage is which table holds the rows behind the source's `table:`, and what
// to say when nothing on this node does.
//
// `table:` names the table a rule reads, so on a sharded cluster it is the
// Distributed table (spec 6.9). That is the right answer for everything asked of
// the table itself: a read of a Distributed table fans out, and its structure
// carries every column. It is the wrong table for the two questions that are
// about stored parts rather than about rows, because a Distributed table has no
// parts and its engine clause has no TTL, so both come back empty and empty is
// what a table with nothing to report looks like.
type storage struct {
	// Source is the table the source names, which is the table a rule reads.
	Source tableRef

	// Local is the table holding the parts, which is Source itself on a single
	// node and the table behind the Distributed engine on a cluster.
	Local tableRef

	// Distributed says Source is a Distributed table and Local is the table
	// behind it. What that costs is worth carrying into the caveat: Local is
	// this node's copy, so an answer read from it is one shard's answer to a
	// question about a cluster.
	Distributed bool

	// Unanswerable says why no local table could be resolved, and is empty when
	// one was. Only a Distributed table sets it. A local table that could not be
	// read from `system.tables` is left silent, because the caller's questions
	// are then the source's own table's and asking it directly is what they
	// already do.
	Unanswerable string
}

// storageTable resolves the source's table to the one holding its parts.
//
// One round trip to `system.tables`, which the user contract in spec 6.7.2
// leaves readable, filtered to the objects the user is granted. A row that is
// not there is therefore as likely to be a grant as a missing table, which is
// why nothing here reports a table that does not exist: the checks that ask
// about the table itself would have failed first and said so.
func (q *Querier) storageTable(ctx context.Context) storage {
	own := tableRef{Database: q.src.Database, Table: q.src.Table}

	var engine, engineFull string
	err := q.conn.QueryRow(ctx,
		"SELECT engine, engine_full FROM system.tables WHERE database = ? AND name = ?",
		own.Database, own.Table).Scan(&engine, &engineFull)
	if err != nil || engine != distributedEngine {
		return storage{Source: own, Local: own}
	}

	local, ok := distributedTarget(engineFull)
	if !ok {
		return storage{
			Source:       own,
			Distributed:  true,
			Unanswerable: "its engine clause does not name a local table this can read",
		}
	}
	return storage{Source: own, Local: local, Distributed: true}
}

// name writes a table the way a caveat names it.
func (r tableRef) name() string { return r.Database + "." + r.Table }

// ttlCaveat says a replay reached past what the table keeps.
//
// On a sharded cluster the retention it names is the local table's, read on the
// node the ruler connects to, because that is where a Distributed table's parts
// are not (spec 6.9). A cluster whose shards were built at different times can
// hold different TTLs, so the caveat says which table answered rather than
// letting a one shard answer read as the cluster's.
func ttlCaveat(ttl time.Duration, st storage) string {
	out := fmt.Sprintf(
		"the range is longer than the %s TTL on %s, so the oldest windows read data the table has "+
			"already deleted", ttl, st.Local.name())

	if st.Distributed {
		out += fmt.Sprintf(" (%s is the local table behind the Distributed %s, read on this node, "+
			"so a shard whose retention differs is not covered)", st.Local.name(), st.Source.name())
	}
	return out
}

// columnCaveat says a column the rule reads arrived inside the range.
//
// Same reservation as the TTL on a sharded cluster, for the same reason: the
// parts it was read from are this node's, so a column an ALTER reached on one
// shard and not another is one shard's answer.
func columnCaveat(added columnAddition, st storage) string {
	out := fmt.Sprintf(
		"the %s column first appears at %s, inside the range, so every window before that read its "+
			"default rather than a value the rule could compare", added.Column, stamp(added.Earliest))

	if st.Distributed {
		out += fmt.Sprintf(" (read from %s, this node's local table behind the Distributed %s, so a "+
			"column added on another shard is not covered)", st.Local.name(), st.Source.name())
	}
	return out
}

// unresolvedCaveat says both questions went unasked.
//
// Reported rather than left silent, because no caveat is what a replay with
// nothing to qualify produces: a check that examined nothing looks exactly like
// one that passed, which is the line spec 7.3 draws for a skipped map key.
func unresolvedCaveat(st storage) string {
	return fmt.Sprintf(
		"%s is a Distributed table and %s, so neither the table's retention nor a column added "+
			"inside the range was checked", st.Source.name(), st.Unanswerable)
}

// distributedEngine is what `system.tables` calls a Distributed table.
const distributedEngine = "Distributed"

// distributedTarget returns the local table a Distributed engine clause names.
//
// Read out of `engine_full` because there is nowhere else to read it: the
// dependency columns of `system.tables` describe materialised views and are
// empty for a Distributed table, and no system table records the arguments of
// an engine. Parsing text for this is affordable only because ClickHouse
// normalises what it stores rather than keeping what the author typed, so
// `Distributed(ruler_shards, currentDatabase(), otel_traces)` comes back as
// three quoted literals with the database already resolved, and the cluster,
// database and table are always the first three.
//
// Anything else answers nothing rather than guessing. An argument that is still
// an expression after the server formatted it is not one this can resolve, and
// the two questions behind this exist to qualify a count: a qualification naming
// the wrong table is worse than none, which is the line parseTTL already draws.
func distributedTarget(engineFull string) (tableRef, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(engineFull), distributedEngine+"(")
	if !ok {
		return tableRef{}, false
	}

	var args [3]string
	for i := range args {
		if i > 0 {
			if rest, ok = strings.CutPrefix(strings.TrimLeft(rest, " "), ","); !ok {
				return tableRef{}, false
			}
		}
		if args[i], rest, ok = quotedLiteral(strings.TrimLeft(rest, " ")); !ok {
			return tableRef{}, false
		}
	}
	if args[1] == "" || args[2] == "" {
		return tableRef{}, false
	}
	return tableRef{Database: args[1], Table: args[2]}, true
}

// quotedLiteral reads one single quoted string from the front of s and returns
// what follows it.
//
// The escaping is the same escaping `EXPLAIN AST` writes a string literal with,
// so unescape decodes it: a quote or a backslash inside a name arrives with a
// backslash in front of it, and the closing quote is the first one that does
// not.
func quotedLiteral(s string) (value, rest string, ok bool) {
	rest, ok = strings.CutPrefix(s, "'")
	if !ok {
		return "", "", false
	}

	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '\'':
			return unescape(rest[:i]), rest[i+1:], true
		case '\\':
			// A trailing backslash means the literal never closed, so there is
			// nothing here to resolve.
			if i+1 >= len(rest) {
				return "", "", false
			}
			i++
		}
	}
	return "", "", false
}
