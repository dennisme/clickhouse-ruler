//go:build integration

package query

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

func assertions(t *testing.T, username string) map[string]Assertion {
	t.Helper()

	src := testSource(t)
	src.Username = username

	q, err := Open(src)
	if err != nil {
		t.Fatalf("Open as %s: %v", username, err)
	}
	t.Cleanup(func() { _ = q.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out := map[string]Assertion{}
	for _, a := range q.Privileges(ctx, lint.Assertions()) {
		out[a.Name] = a
	}
	if len(out) != len(lint.Assertions()) {
		t.Fatalf("got %d assertions, want %d", len(out), len(lint.Assertions()))
	}
	return out
}

// The reference user in deploy/clickhouse/init/02-ruler-user.sql is the
// contract in spec 6.7.2 as something that runs. If this fails, either the
// contract changed or the user did.
func TestPrivilegesCompliantUser(t *testing.T) {
	for name, got := range assertions(t, "ruler_payments") {
		if got.Status != StatusPass {
			t.Errorf("%s = %v: %s", name, got.Status, got.Detail)
		}
	}
}

// The failure this check exists for, and the reason it cannot be left to the
// operator to notice: every rule evaluates perfectly as this user. Nothing
// about an evaluation looks different, so without a probe nobody finds out
// until someone writes the query that reads around the row policies.
func TestPrivilegesOverPrivilegedUser(t *testing.T) {
	got := assertions(t, "ruler_wide")

	want := map[string]Status{
		lint.AssertionSourcesRevoked: StatusFail,
		lint.AssertionReadonly:       StatusFail,
		lint.AssertionConstraints:    StatusFail,

		// The one thing this user does satisfy. Too many grants and too few
		// are different findings, and a check that conflated them would say
		// the wrong thing about half the clusters it runs on.
		lint.AssertionTableReadable: StatusPass,
	}

	for name, wantStatus := range want {
		if got[name].Status != wantStatus {
			t.Errorf("%s = %v (%s), want %v", name, got[name].Status, got[name].Detail, wantStatus)
		}
	}

	// A finding has to name what is granted, or an operator knows only that
	// something is wrong with a user they did not write down in full.
	if detail := got[lint.AssertionSourcesRevoked].Detail; !strings.Contains(detail, "URL") {
		t.Errorf("sources-revoked detail = %q, want it to name the privilege", detail)
	}
}

// An unreachable endpoint is what every probe names, so the privileges have
// to be what decides the result rather than whether the probe could connect.
// The over-privileged user proves it: its url() probe fails on a refused
// connection, after the privilege check has already let it through, and that
// still reports as granted.
func TestPrivilegesReadsPastAFailedProbeQuery(t *testing.T) {
	got := assertions(t, "ruler_wide")[lint.AssertionSourcesRevoked]

	if got.Status != StatusFail {
		t.Fatalf("status = %v (%s), want fail", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "the privilege is granted") {
		t.Errorf("detail = %q, want it to say the probe got past the privilege check", got.Detail)
	}
}

// Requiring fewer assertions runs fewer probes. An operator on a managed
// cluster drops the one they cannot satisfy and keeps the rest (spec 7.6).
func TestPrivilegesRunsOnlyWhatIsRequired(t *testing.T) {
	src := testSource(t)

	q, err := Open(src)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got := q.Privileges(ctx, []string{lint.AssertionTableReadable})
	if len(got) != 1 || got[0].Name != lint.AssertionTableReadable {
		t.Fatalf("got %v, want only table-readable", got)
	}
}

// The under-granted direction. A user who cannot read the source's own table
// fails every evaluation, and this is what makes that a finding in CI rather
// than an ACCESS_DENIED on the first tick.
func TestPrivilegesUnderGrantedUser(t *testing.T) {
	src := testSource(t)
	src.Table = "does_not_exist_for_this_user"

	q, err := Open(src)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = q.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got := q.Privileges(ctx, []string{lint.AssertionTableReadable})
	if len(got) != 1 {
		t.Fatalf("got %v, want one assertion", got)
	}
	if got[0].Status == StatusPass {
		t.Errorf("status = pass, want a finding: the user cannot read %s", src.Table)
	}
}
