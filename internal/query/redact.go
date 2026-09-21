package query

import (
	"fmt"
	"strings"

	"github.com/dennisme/clickhouse-ruler/internal/rule"
)

// redact removes a password from text before it is returned or logged.
//
// Connection options are built from fields rather than a DSN, so a driver
// error has no reason to contain the credential. This is a backstop for the
// cases where it does anyway, since an error message is the easiest way for a
// secret to reach a log.
func redact(text, password string) string {
	if password == "" {
		return text
	}
	return strings.ReplaceAll(text, password, "xxxxx")
}

// queryErr attributes a driver error to the rule that caused it, with the
// source's password removed. The scheduler logs an evaluation failure, so
// anything the driver says about the connection reaches a log; the address and
// the database are what an operator needs to read there, the credential is
// not.
//
// The driver error is not wrapped, because unwrapping it would hand a caller
// the unredacted text this exists to remove.
func (q *Querier) queryErr(r rule.Rule, err error) error {
	return fmt.Errorf("rule %q: %s", r.Alert, redact(err.Error(), q.src.Password))
}
