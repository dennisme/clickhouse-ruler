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

// Reload re-reads the files the ruler was started with and reports whether the
// new version is the one now running. A nil error means it is; an error is the
// refusal's reason, and the previous version keeps running (spec 7.6).
//
// Supplied by whoever owns the loading, and nil when the operator did not ask
// for the endpoint.
type Reload func(ctx context.Context) error

// Handler is the complete HTTP surface from spec 8.1: metrics, the two health
// endpoints, and a reload when one was wired. There is no rule create, update or
// delete endpoint, and there never will be: the absence of a write path is the
// security model, and a reload is not a write path because the caller supplies
// nothing and the files are the ones already on disk.
func Handler(reg *prometheus.Registry, ready Ready, reload Reload) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/-/healthy", healthy)
	mux.HandleFunc("/-/ready", readiness(ready))

	// Absent rather than disabled when nobody asked for it, so a scan of the
	// surface finds what the surface is.
	if reload != nil {
		mux.HandleFunc("/-/reload", reloading(reload))
	}
	return mux
}

// reloading re-reads the files on request, for the deployments a signal cannot
// reach: a sidecar holding a synced rules repository has the files and no way to
// signal the process beside it (spec 8.1).
//
// POST only. A reload changes which rules the process is evaluating, so it is
// not something a link, a crawler or a readiness prober can do by accident.
func reloading(reload Reload) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			_, _ = w.Write([]byte("reload is POST only\n"))
			return
		}

		// The reason travels in the body, as it does for readiness: a caller
		// that has to read the ruler's logs to learn whether its own request
		// worked is no better off than with the signal this replaces. The
		// ruler is not failing, so this is 500 for the request rather than a
		// state anything should alert on: the previous configuration is still
		// evaluating and still paging.
		if err := reload(r.Context()); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("reload refused, the previous configuration keeps running: " + err.Error() + "\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reloaded\n"))
	}
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
