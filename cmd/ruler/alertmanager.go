package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/scheduler"
)

// How often each configured Alertmanager is asked whether it answers, and how
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

// parseAlertmanagerURL reads one value of the --alertmanager flag.
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

// alertmanagerFlag collects a repeated --alertmanager, in the order it was
// given, because the flag package keeps only the last value of a plain string
// flag.
type alertmanagerFlag []string

func (f *alertmanagerFlag) String() string { return strings.Join(*f, ",") }

func (f *alertmanagerFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// parseAlertmanagerURLs reads every value of the repeated --alertmanager.
//
// Every value goes through the same parsing, because the second address being a
// typo is no less silent than the first one being one.
//
// The same address twice is refused. It is the same page posted twice to one
// member, which Alertmanager deduplicates, so nothing breaks and nothing is
// gained; what it does break is the metrics, because the `alertmanager` label is
// the redacted URL and two identical values are one series, so a failure counter
// would report two endpoints as one. Deduplicating quietly would leave an
// operator with a flag list that does not describe what the ruler is doing
// (spec 6.5).
func parseAlertmanagerURLs(values []string) ([]*url.URL, error) {
	urls := make([]*url.URL, 0, len(values))
	seen := make(map[string]struct{}, len(values))

	for _, v := range values {
		u, err := parseAlertmanagerURL(v)
		if err != nil {
			return nil, err
		}

		// A trailing slash is the same endpoint: notify.Client trims one
		// before it builds a request path.
		key := strings.TrimSuffix(u.String(), "/")
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("--alertmanager %q: given twice, list each member of the cluster once",
				u.Redacted())
		}
		seen[key] = struct{}{}

		urls = append(urls, u)
	}
	return urls, nil
}

// alertmanagerEndpoint is one member of the cluster alerts go to: the client
// that posts and probes, and the redacted spelling that labels its series and
// names it in a log line (spec 8.4).
type alertmanagerEndpoint struct {
	url    string
	client *notify.Client
}

func alertmanagerEndpoints(urls []*url.URL) []alertmanagerEndpoint {
	endpoints := make([]alertmanagerEndpoint, 0, len(urls))
	for _, u := range urls {
		endpoints = append(endpoints, alertmanagerEndpoint{
			url:    u.Redacted(),
			client: notify.NewClient(u.String()),
		})
	}
	return endpoints
}

// sendEndpoints is what a send is fanned out across: every endpoint, each
// counting its own deliveries and failures (spec 6.5).
func sendEndpoints(endpoints []alertmanagerEndpoint) []scheduler.Endpoint {
	out := make([]scheduler.Endpoint, 0, len(endpoints))
	for _, e := range endpoints {
		out = append(out, scheduler.Endpoint{Sender: e.client, URL: e.url})
	}
	return out
}

// newAlertmanagerProbes builds one probe per endpoint. Each reports its own
// gauge series, so a cluster with one member down says which member, and each
// runs on its own timer, so one member that hangs delays nobody else's reading
// (spec 6.5, 8.2).
func newAlertmanagerProbes(endpoints []alertmanagerEndpoint, metrics *scheduler.Metrics, log *slog.Logger) []*alertmanagerProbe {
	probes := make([]*alertmanagerProbe, 0, len(endpoints))
	for _, e := range endpoints {
		probes = append(probes, &alertmanagerProbe{
			client:   e.client,
			url:      e.url,
			metrics:  metrics,
			log:      log,
			interval: probeInterval,
			timeout:  probeTimeout,
		})
	}
	return probes
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

// alertmanagerProbe asks one Alertmanager whether it answers, on its own timer,
// and reports the answer as a gauge series of its own and on a change of state
// as a log line.
//
// Neither a readiness term nor a startup refusal: an Alertmanager is one service
// every replica points at, so either would turn a rolling restart of it into a
// fleet of rulers that cannot be restarted or cannot pass a rollout, for an
// outage a delivery retry already rides out, and with a list of endpoints a
// member being down is what the other members are for (spec 8.1, 6.5).
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
