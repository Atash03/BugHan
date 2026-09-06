package httpapi

import "net/http"

// registerIngest mounts the SDK-facing ingest endpoints. The full pipeline
// lands in the ingest slice; the routes exist from the start so SDKs get a
// definitive 501 rather than a 404 while bootstrapping.
func (s *Server) registerIngest(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/{projectID}/envelope/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "ingest not implemented yet", http.StatusNotImplemented)
	})
}
