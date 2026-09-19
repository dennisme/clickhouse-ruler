package query

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
	"github.com/dennisme/clickhouse-ruler/internal/rule"
	"github.com/dennisme/clickhouse-ruler/internal/source"
)

// Querier runs a source's rules against ClickHouse.
type Querier struct {
	src  source.Source
	conn driver.Conn
}

// Open connects to a source. The caller closes the Querier.
//
// Connection options are built field by field rather than from a DSN string.
// A DSN would carry the password through string handling, where any error
// that echoes the input puts the credential in a log.
func Open(src source.Source) (*Querier, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{src.Address},
		Auth: clickhouse.Auth{
			Database: src.Database,
			Username: src.Username,
			Password: src.Password,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("source %q: connecting: %s", src.Name, redact(err.Error(), src.Password))
	}
	return &Querier{src: src, conn: conn}, nil
}

func (q *Querier) Close() error { return q.conn.Close() }

func (q *Querier) Ping(ctx context.Context) error { return q.conn.Ping(ctx) }

// Run evaluates one rule and returns a sample per returned row.
func (q *Querier) Run(ctx context.Context, r rule.Rule, now time.Time) ([]alert.Sample, error) {
	sql, err := render(r.Expr)
	if err != nil {
		return nil, fmt.Errorf("rule %q: %w", r.Alert, err)
	}
	from, to := window(q.src, r, now)

	ctx = clickhouse.Context(ctx,
		clickhouse.WithParameters(clickhouse.Parameters{
			"from": from.UTC().Format("2006-01-02 15:04:05.000"),
			"to":   to.UTC().Format("2006-01-02 15:04:05.000"),
		}),
		clickhouse.WithSettings(clickhouse.Settings{
			"max_execution_time": int(q.src.MaxExecutionTime.Seconds()),
			"max_memory_usage":   q.src.MaxMemoryUsage,
			// The client side cap in toSamples protects the ruler's memory.
			// This one makes the server stop producing rows in the first
			// place, so a runaway rule costs the cluster nothing either.
			"max_result_rows":      q.src.MaxRows + 1,
			"result_overflow_mode": "throw",
		}),
	)

	rows, err := q.conn.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("rule %q: %w", r.Alert, err)
	}
	// Close reports errors already surfaced by rows.Err below.
	defer func() { _ = rows.Close() }()

	columns := rows.Columns()
	types := rows.ColumnTypes()

	var collected [][]any
	for rows.Next() {
		scan := make([]any, len(columns))
		for i := range scan {
			scan[i] = reflect.New(types[i].ScanType()).Interface()
		}
		if err := rows.Scan(scan...); err != nil {
			return nil, fmt.Errorf("rule %q: scanning row: %w", r.Alert, err)
		}

		values := make([]any, len(scan))
		for i, p := range scan {
			values[i] = reflect.ValueOf(p).Elem().Interface()
		}
		collected = append(collected, values)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rule %q: %w", r.Alert, err)
	}

	samples, err := toSamples(columns, collected, q.src.MaxRows)
	if err != nil {
		return nil, fmt.Errorf("rule %q: %w", r.Alert, err)
	}
	return samples, nil
}
