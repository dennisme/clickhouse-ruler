package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/dennisme/clickhouse-ruler/internal/notify"
	"github.com/dennisme/clickhouse-ruler/internal/scheduler"
	"github.com/dennisme/clickhouse-ruler/internal/source"
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

// alertmanagerEndpoint is one member of the cluster alerts go to: the client
// that posts and probes, and the URL that labels its series and names it in a
// log line.
//
// The URL itself rather than a redacted spelling of it. A URL carrying
// userinfo is refused when the operator's file is read, so no URL that reaches
// here can hold a secret and there is nothing left to redact (spec 6.5, 8.4).
type alertmanagerEndpoint struct {
	url    string
	client *notify.Client
}

// alertmanagerEndpoints builds one client per member of the set.
//
// The credential is resolved once here and set on each client, because every
// member of one cluster shares it: the set is the cluster, and its auth is a
// property of the cluster rather than of an address (spec 6.5).
func alertmanagerEndpoints(set source.Alertmanager) []alertmanagerEndpoint {
	credential, _ := set.Credential()

	endpoints := make([]alertmanagerEndpoint, 0, len(set.URLs))
	for _, u := range set.URLs {
		client := notify.NewClient(u)
		client.SetAuthorization(credential)
		endpoints = append(endpoints, alertmanagerEndpoint{url: u, client: client})
	}
	return endpoints
}

// rotateCredential applies a freshly read credential to every endpoint.
//
// What a reload does to this block, and the whole of it. The URLs are topology
// and are read once at startup, because each one owns a probe goroutine and a
// gauge series whose lifecycle a changing list would have to manage; the
// credential is a secret and rotates in place, which is the same split 6.2
// draws between a CA that needs a reload and a client certificate that does
// not (spec 6.5).
func rotateCredential(endpoints []alertmanagerEndpoint, set source.Alertmanager) {
	credential, _ := set.Credential()
	for _, e := range endpoints {
		e.client.SetAuthorization(credential)
	}
}

// sameAlertmanagerURLs reports whether a freshly read set names the endpoints
// already running, so a reload can say that a changed list needs a restart
// rather than appearing to apply one.
func sameAlertmanagerURLs(endpoints []alertmanagerEndpoint, set source.Alertmanager) bool {
	if len(endpoints) != len(set.URLs) {
		return false
	}
	for i, e := range endpoints {
		if e.url != set.URLs[i] {
			return false
		}
	}
	return true
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
