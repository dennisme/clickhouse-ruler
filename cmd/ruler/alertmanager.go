package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/scheduler"
)

// How often the configured Alertmanager is asked whether it answers, and how
// long one of those requests gets (spec 8.2).
//
// Fixed rather than flags. Thirty seconds is a scrape interval, and nothing
// about a deployment makes either number theirs to choose: a shorter interval
// buys no earlier warning that matters, and a longer one is the signal arriving
// after the page it was meant to precede. The timeout is well inside the
// interval, so a probe that hangs cannot overlap the next one.
const (
	probeInterval = 30 * time.Second
	probeTimeout  = 5 * time.Second
)

// parseAlertmanagerURL reads the --alertmanager flag.
//
// An unparseable URL is refused at startup rather than handed to the notify
// client, which is the precedent parseLogLevel sets and for the same reason:
// `--alertmanager localhost:9093` builds localhost:9093/api/v2/alerts, and
// net/http refuses that at the first send, which may be hours later and is the
// first page the ruler was ever asked to deliver (spec 8.1).
//
// Three things make it malformed, and each of them is a URL no send could ever
// succeed against: no scheme, a scheme net/http will not speak, or no host to
// send to. Reachability is not one of them, because that needs the network and
// changes while the ruler runs.
func parseAlertmanagerURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		// The error from url.Parse prints the value it was given, userinfo
		// and all, so only its reason is repeated here (spec 8.4).
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, fmt.Errorf("--alertmanager is not a URL: %w, want one like http://localhost:9093", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("--alertmanager %q: want a http:// or https:// URL, e.g. http://localhost:9093", u.Redacted())
	}
	if u.Host == "" {
		return nil, fmt.Errorf("--alertmanager %q: no host to send alerts to, want one like http://localhost:9093", u.Redacted())
	}
	return u, nil
}

// prober is all the probe loop asks of a notify client: whether Alertmanager
// answers. notify.Client is one.
//
// The interface is here rather than in internal/notify for the reason queryCost
// is: the loop reports into a Prometheus gauge, and internal/notify does not
// learn what a collector is (spec 8.2).
type prober interface {
	Probe(ctx context.Context) error
}

// alertmanagerProbe asks the configured Alertmanager whether it answers, on its
// own timer, and reports the answer as a gauge and on a change of state as a
// log line.
//
// Neither a readiness term nor a startup refusal: an Alertmanager is one
// service every replica points at, so either would turn a rolling restart of it
// into a fleet of rulers that cannot be restarted or cannot pass a rollout, for
// an outage a delivery retry already rides out (spec 8.1, 6.5).
type alertmanagerProbe struct {
	client  prober
	url     string
	metrics *scheduler.Metrics
	log     *slog.Logger

	interval time.Duration
	timeout  time.Duration
}

// run probes until ctx is cancelled, starting immediately so the gauge has a
// reading from startup rather than one interval later. That first probe is what
// makes a typo'd host visible before anything fires.
func (p *alertmanagerProbe) run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	// Nothing has failed yet, so a first success is not a recovery and writes
	// no line.
	answering := true
	for {
		answering = p.probeOnce(ctx, answering)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// probeOnce probes and reports, returning whether Alertmanager answered so the
// caller can hand it back as the previous state.
//
// A line on a change of state and not on every probe: a probe every thirty
// seconds is nearly three thousand lines a day against an Alertmanager that is
// down, which buries the line saying when it went down, while the gauge beside
// it already answers whether it is answering now (spec 8.4).
func (p *alertmanagerProbe) probeOnce(ctx context.Context, was bool) bool {
	// A shutdown cancels this context, and the request it cuts off is not an
	// Alertmanager that stopped answering: reporting one would put a warning
	// and a zero on every clean shutdown.
	if ctx.Err() != nil {
		return was
	}

	probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	err := p.client.Probe(probeCtx)
	if ctx.Err() != nil {
		return was
	}

	if err != nil {
		p.metrics.AlertmanagerLastProbeSuccessful.WithLabelValues(p.url).Set(0)
		if was {
			// The request error is what says whether the host does not
			// resolve, the port refuses, or Alertmanager answered and is not
			// ready. It carries no credentials: notify removes them.
			p.log.Warn("the alertmanager did not answer its probe",
				"alertmanager", p.url, "error", err.Error())
		}
		return false
	}

	p.metrics.AlertmanagerLastProbeSuccessful.WithLabelValues(p.url).Set(1)
	if !was {
		p.log.Info("the alertmanager answered its probe again", "alertmanager", p.url)
	}
	return true
}
