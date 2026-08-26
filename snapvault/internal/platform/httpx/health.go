package httpx

import "net/http"

// Health registers liveness and readiness endpoints on mux.
//
//   - /health/live  reports that the process is up (used by Kubernetes to
//     decide whether to restart the container).
//   - /health/ready reports that the service can serve traffic (used to
//     decide whether to route requests to it). ready is a function so a
//     service can report "not ready" while a dependency is unavailable.
//
// Pass ready == nil for a service that is ready as soon as it is live.
func Health(mux *http.ServeMux, ready func() bool) {
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, _ *http.Request) {
		if ready == nil || ready() {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	})
}
