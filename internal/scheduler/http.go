package scheduler

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// readyTimeout bounds what a readiness probe may do. A probe holds a
// supervisor's request open, so a cluster that has stopped answering must
// make the check fail rather than make it hang.
const readyTimeout = 5 * time.Second

// Ready answers whether sending traffic to this ruler is useful: rules
// loaded, and at least one source answering. A nil error means ready, and
// the error is the reason the probe reports (spec 8.1).
//
// A function rather than an interface over the queriers, because the answer
// is assembled by whoever opened them and nothing else here needs to know
// how it was reached.
type Ready func(ctx context.Context) error

// Handler is the complete HTTP surface from spec 8.1: metrics, and the two
// health endpoints. There is no rule create, update or delete endpoint, and
// there never will be: the absence of a write path is the security model.
func Handler(reg *prometheus.Registry, ready Ready) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/-/healthy", healthy)
	mux.HandleFunc("/-/ready", readiness(ready))
	return mux
}

// healthy is about the process. It is alive and its listener is serving,
// which is what answering at all already says. A failed health check gets a
// process restarted, and a ruler that cannot reach its cluster is not
// something a restart fixes (spec 8.1).
func healthy(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// readiness is about whether sending traffic here is useful, which is a
// different question and wired to different things: a failed readiness probe
// takes a ruler out of a load balancer or stops a rollout replacing working
// replicas with broken ones (spec 8.1, 10.2).
//
// The reason travels in the body. A probe that says only "not ready" sends
// whoever is rolling out to the logs for something the ruler already knows.
func readiness(ready Ready) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()

		if err := ready(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready: " + err.Error()))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
