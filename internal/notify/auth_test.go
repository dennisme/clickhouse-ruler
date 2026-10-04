package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
)

// headerSpy records the Authorization header of every request it answers, so a
// test can assert what went on the wire rather than what was configured.
func headerSpy(t *testing.T, status int) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestSendAuthenticates(t *testing.T) {
	srv, seen := headerSpy(t, http.StatusOK)

	c := NewClient(srv.URL)
	c.SetAuthorization("Basic cnVsZXI6aHVudGVyMg==")

	if err := c.Send(context.Background(), []alert.Alert{firingAlert()}); err != nil {
		t.Fatalf("Send = %v, want nil", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("got %d requests, want 1", len(*seen))
	}
	if (*seen)[0] != "Basic cnVsZXI6aHVudGVyMg==" {
		t.Errorf("Authorization = %q, want the configured credential", (*seen)[0])
	}
}

// The probe authenticates too. An Alertmanager behind basic auth answers
// /-/ready with a 401 to an anonymous request, so a probe that did not
// authenticate would report a healthy Alertmanager as unreachable for the life
// of the process (spec 8.2).
func TestProbeAuthenticates(t *testing.T) {
	srv, seen := headerSpy(t, http.StatusOK)

	c := NewClient(srv.URL)
	c.SetAuthorization("Bearer sekrit")

	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("Probe = %v, want nil", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("got %d requests, want 1", len(*seen))
	}
	if (*seen)[0] != "Bearer sekrit" {
		t.Errorf("Authorization = %q, want the configured credential", (*seen)[0])
	}
}

// A client with no credential sends no header, rather than an empty one: an
// Alertmanager that is not behind auth should see the request it saw before
// this field existed.
func TestNoCredentialSendsNoHeader(t *testing.T) {
	srv, seen := headerSpy(t, http.StatusOK)

	if err := NewClient(srv.URL).Send(context.Background(), []alert.Alert{firingAlert()}); err != nil {
		t.Fatalf("Send = %v, want nil", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("got %d requests, want 1", len(*seen))
	}
	if (*seen)[0] != "" {
		t.Errorf("Authorization = %q, want no header", (*seen)[0])
	}
}

// A 401 is not retried. The credential will not have changed by the next
// attempt, so three more requests buy nothing and delay the report that the
// credential is wrong.
func TestUnauthorizedIsNotRetried(t *testing.T) {
	srv, seen := headerSpy(t, http.StatusUnauthorized)

	c := NewClient(srv.URL)
	if err := c.Send(context.Background(), []alert.Alert{firingAlert()}); err == nil {
		t.Fatal("Send = nil against a 401, want a failure")
	}
	if len(*seen) != 1 {
		t.Errorf("got %d requests, want 1: a 401 is not worth repeating", len(*seen))
	}
}

// The credential never reaches an error. A failed send is logged with the
// error as a field, so a credential in it is a credential in the log
// (spec 8.4).
func TestErrorsNeverCarryTheCredential(t *testing.T) {
	srv, _ := headerSpy(t, http.StatusUnauthorized)

	c := NewClient(srv.URL)
	c.SetAuthorization("Basic cnVsZXI6aHVudGVyMg==")

	err := c.Send(context.Background(), []alert.Alert{firingAlert()})
	if err == nil {
		t.Fatal("Send = nil against a 401, want a failure")
	}
	for _, leak := range []string{"cnVsZXI6aHVudGVyMg==", "Basic "} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q carries the credential", err)
		}
	}
}
