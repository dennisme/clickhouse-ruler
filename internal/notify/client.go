package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/alert"
)

const alertsPath = "/api/v2/alerts"

// Defaults chosen so a rolling Alertmanager restart is ridden out rather than
// dropping a page, without holding an evaluation for long.
const (
	DefaultMaxAttempts = 4
	DefaultBackoff     = 250 * time.Millisecond
	DefaultTimeout     = 10 * time.Second
)

// Client posts alerts to Alertmanager.
type Client struct {
	URL  string
	HTTP *http.Client

	// MaxAttempts counts the first try, so 1 disables retrying.
	MaxAttempts int

	// Backoff is the first retry delay and doubles each attempt.
	Backoff time.Duration
}

func NewClient(url string) *Client {
	return &Client{
		URL:         strings.TrimSuffix(url, "/"),
		HTTP:        &http.Client{Timeout: DefaultTimeout},
		MaxAttempts: DefaultMaxAttempts,
		Backoff:     DefaultBackoff,
	}
}

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

	resp, err := c.HTTP.Do(req)
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

// retryableError marks a failure worth repeating with the same payload.
type retryableError struct{ error }

func (e retryableError) Unwrap() error { return e.error }

func retryable(err error) bool {
	var r retryableError
	return errors.As(err, &r)
}
