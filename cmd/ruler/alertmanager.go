package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"reflect"
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

	// tls is the material this client's transport was built from, kept so a
	// reload can tell a bundle that moved from one nobody edited and rebuild
	// only for the first. The bytes compare where a built tls.Config cannot: a
	// certificate pool holds a closure per certificate and two closures are
	// never equal (spec 6.5, 6.2).
	tls *source.TLS
}

// alertmanagerEndpoints builds one client per member of the set.
//
// The credential is resolved once here and set on each client, because every
// member of one cluster shares it: the set is the cluster, and its auth is a
// property of the cluster rather than of an address (spec 6.5).
// The TLS material is resolved once for the same reason: the set is the
// cluster, so every member is reached over the same trust.
//
// An error from Config is what parsing already reported as an
// alertmanager/tls finding, which refuses the start, so the client is left on
// the host's trust store rather than this discovering it a second time.
func alertmanagerEndpoints(set source.Alertmanager) []alertmanagerEndpoint {
	credential, _ := set.Credential()

	endpoints := make([]alertmanagerEndpoint, 0, len(set.URLs))
	for _, u := range set.URLs {
		client := notify.NewClient(u)
		client.SetAuthorization(credential)
		endpoints = append(endpoints, alertmanagerEndpoint{url: u, client: client})
	}

	// Only when the set configured it, so a plaintext Alertmanager keeps the
	// process-wide transport and its connection pool rather than a clone of
	// its own.
	if set.TLS != nil {
		_ = rotateTLS(endpoints, set)
	}
	return endpoints
}

// rotateCredential applies a freshly read credential to every endpoint.
//
// The URLs are topology and are read once at startup, because each one owns a
// probe goroutine and a gauge series whose lifecycle a changing list would
// have to manage. The credential is a secret and rotates in place (spec 6.5).
func rotateCredential(endpoints []alertmanagerEndpoint, set source.Alertmanager) {
	credential, _ := set.Credential()
	for _, e := range endpoints {
		e.client.SetAuthorization(credential)
	}
}

// rotateTLS rebuilds every endpoint's transport from freshly read material and
// records what it was built from, so the next reload compares against what is
// running.
//
// By index, because the material is a field of the endpoint rather than of the
// client behind it, and a copy would leave the slice holding the bundle that
// has just been replaced.
//
// The error is what parsing already reported as an alertmanager/tls finding,
// which fails config.deliverable and returns before this is reached, so it is
// returned rather than ignored only so that a reader is not left wondering
// which of the two places decides.
func rotateTLS(endpoints []alertmanagerEndpoint, set source.Alertmanager) error {
	var transport *tls.Config
	if set.TLS != nil {
		var err error
		if transport, err = set.TLS.Config(); err != nil {
			return err
		}
	}

	// A nil config is a tls_config that was removed, which leaves the endpoint
	// on the host's trust store. Applied rather than skipped: the urls cannot
	// move under a reload, so an https endpoint whose block went away is an
	// operator asking for exactly that.
	for i := range endpoints {
		endpoints[i].client.SetTLS(transport)
		endpoints[i].tls = set.TLS
	}
	return nil
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

// sameAlertmanagerTLS reports whether a freshly read set names the material the
// endpoints already running were built with.
//
// The CA is the half that cannot move without rebuilding a transport, and the
// client pair is held as the two paths and read at each handshake, so this
// compares the whole block and a rotated pair compares equal (spec 6.5).
func sameAlertmanagerTLS(endpoints []alertmanagerEndpoint, set source.Alertmanager) bool {
	for _, e := range endpoints {
		if !reflect.DeepEqual(e.tls, set.TLS) {
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
