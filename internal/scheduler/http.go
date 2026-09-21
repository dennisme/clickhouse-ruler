package scheduler

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Handler is the complete HTTP surface from spec 8.1: metrics, and the two
// health endpoints. There is no rule create, update or delete endpoint, and
// there never will be: the absence of a write path is the security model.
func Handler(reg *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/-/healthy", ok)
	mux.HandleFunc("/-/ready", ok)
	return mux
}

func ok(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
