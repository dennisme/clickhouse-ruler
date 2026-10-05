package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
)

const alertsPath = "/api/v2/alerts"

// readyPath is Alertmanager's own readiness endpoint. The question a probe
// asks is whether it would accept an alert now, not whether a socket opens
// (spec 8.2).
const readyPath = "/-/ready"

// Defaults chosen so a rolling Alertmanager restart is ridden out rather than
// dropping a page, without holding an evaluation for long.
const (
	DefaultMaxAttempts = 4
	DefaultBackoff     = 250 * time.Millisecond
	DefaultTimeout     = 10 * time.Second
)

// Client posts alerts to Alertmanager.
type Client struct {
	URL string

	// client is what every send and every probe goes through.
	//
	// Atomic for the reason auth below is, and for a harder version of it. A
	// CA lives inside a built transport and crypto/tls offers no callback for
	// its roots, so applying a replaced bundle means replacing this whole
	// client while the delivery worker and this endpoint's probe are using it
	// (spec 6.5).
	client atomic.Pointer[http.Client]

	// auth is the Authorization header value sent on every request, unset for
	// an Alertmanager that is not behind auth. It is built from the credential
	// the operator's file named, never from the URL (spec 6.5).
	//
	// A resolved header rather than a username and a password, because
	// basic_auth and a bearer token differ only in how this string is spelled,
	// and the client has no reason to know which it was given.
	//
	// Atomic rather than a plain field because a reload rotates it from the
	// run loop while the delivery worker and this endpoint's probe are both
	// using the client. Swapping the value in place rather than rebuilding the
	// client is what keeps the Cadence, the send queue and the probe goroutine
	// untouched by a rotation (spec 6.5).
	auth atomic.Pointer[string]

	// MaxAttempts counts the first try, so 1 disables retrying.
	MaxAttempts int

	// Backoff is the first retry delay and doubles each attempt.
	Backoff time.Duration
}

func NewClient(url string) *Client {
	c := &Client{
		URL:         strings.TrimSuffix(url, "/"),
		MaxAttempts: DefaultMaxAttempts,
		Backoff:     DefaultBackoff,
	}
	c.client.Store(&http.Client{Timeout: DefaultTimeout})
	return c
}

// httpClient is the client this request is made with, read once so a rotation
// arriving part way through a send cannot move it underneath the request.
func (c *Client) httpClient() *http.Client { return c.client.Load() }

// Send renders and posts every alert worth notifying about.
//
// Returning an error reports that notification failed; it does not mean the
// evaluation failed. The caller keeps its alert state either way, otherwise an
// Alertmanager outage would also lose the `for` timers that survived it.
func (c *Client) Send(ctx context.Context, alerts []alert.Alert) error {
	payload := Payload(alerts)
	// Every quiet evaluation of every rule would otherwise post an empty
	// array, which is almost all evaluations.
	if len(payload) == 0 {
		return nil
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding alerts: %w", err)
	}

	backoff := c.Backoff
	attempts := c.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		lastErr = c.post(ctx, body)
		if lastErr == nil {
			return nil
		}
		// A 4xx means this payload is wrong, so the identical retry would be
		// wrong too. Only a 5xx or a transport failure is worth repeating.
		if !retryable(lastErr) || attempt == attempts {
			break
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return fmt.Errorf("sending to alertmanager: %w", lastErr)
}

func (c *Client) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+alertsPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authenticate(req)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return retryableError{err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 500 {
		return retryableError{fmt.Errorf("alertmanager returned %s", resp.Status)}
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("alertmanager returned %s", resp.Status)
	}
	return nil
}

// Probe reports whether the configured Alertmanager answers.
//
// One request, no retry: the caller probes on a timer, so the next tick is the
// retry, and a probe that retried would report a 25 second outage as a healthy
// reading (spec 8.2). It sends no alert and touches no alert state, so a
// failure here is a fact about the address rather than about a notification.
func (c *Client) Probe(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+readyPath, nil)
	if err != nil {
		return err
	}
	// The probe authenticates too. An Alertmanager behind basic auth answers
	// /-/ready with a 401 to an anonymous request, so a probe that skipped the
	// header would report a healthy Alertmanager as unreachable for the life
	// of the process (spec 8.2).
	c.authenticate(req)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("alertmanager returned %s", resp.Status)
	}
	return nil
}

// SetTLS builds the transport this client posts and probes over, and replaces
// the one it was using.
//
// A rotation, the way SetAuthorization is, so a replaced ca_file costs a
// signal rather than a restart. The difference is what has to move: a
// credential is a header value, while roots live inside a built transport and
// crypto/tls offers no callback for them, so the whole client is swapped
// (spec 6.5).
//
// Safe while sends and probes are in flight. A request already reading the
// client finishes on it, which is one request made with the previous roots
// rather than a request lost, the rule the credential already follows.
//
// The client pair rotates without this being called at all, because cfg reads
// it at each handshake.
func (c *Client) SetTLS(cfg *tls.Config) {
	// The timeout stays the client's. A transport with no bound would make a
	// send wait on an Alertmanager that accepted a connection and stopped,
	// which is the hang the retry ladder is sized against (spec 6.5).
	// Cloned from the default rather than built empty, so an estate that
	// reaches Alertmanager through HTTPS_PROXY keeps doing so and the
	// connection pool keeps the sizes everything else in this process uses.
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return
	}
	transport = transport.Clone()
	transport.TLSClientConfig = cfg

	previous := c.httpClient()
	c.client.Store(&http.Client{Timeout: previous.Timeout, Transport: transport})

	// The replaced transport is dropped, and dropping it is not enough to put
	// its connections down: each pooled connection has a goroutine of its own
	// holding the transport, so nothing collects it and the sockets verified
	// against the previous roots stay open for the life of the process. The
	// ruler probes every thirty seconds, so there is always one of them.
	//
	// Closed after the store, so a request still in flight keeps the
	// connection it is using and only the idle ones go. The requests that
	// start next were handed the transport above.
	if t, ok := previous.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

// SetAuthorization replaces the credential every later request authenticates
// with. An empty value means no header at all, which is the Alertmanager that
// is not behind auth.
//
// Safe while sends and probes are in flight. A request already built keeps the
// credential it was built with, which is one request posted with the previous
// secret rather than a request lost.
func (c *Client) SetAuthorization(value string) { c.auth.Store(&value) }

// authenticate adds the credential, if there is one. Set rather than added, so
// a retry of the same request cannot stack two headers.
func (c *Client) authenticate(req *http.Request) {
	value := c.auth.Load()
	if value == nil || *value == "" {
		return
	}
	req.Header.Set("Authorization", *value)
}

// retryableError marks a failure worth repeating with the same payload.
type retryableError struct{ error }

func (e retryableError) Unwrap() error { return e.error }

func retryable(err error) bool {
	var r retryableError
	return errors.As(err, &r)
}
