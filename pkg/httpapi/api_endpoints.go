package httpapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// The API reference, generated from the route table rather than written beside
// it.
//
// docs/api.md is hand-maintained and the route table has grown past ninety
// entries; any list written by hand is wrong within a month, and a wrong API
// reference is worse than none because it sends people to endpoints that do
// not exist. So the routes are recorded as they are registered, which makes
// drift impossible by construction rather than by discipline.

// Endpoint is one registered route.
type Endpoint struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// routeRecorder wraps *http.ServeMux and notes every pattern registered
// through it.
//
// It is deliberately shaped so that RegisterRoutes can keep calling
// `mux.HandleFunc(...)` unchanged: the recorder shadows the parameter, so
// adding a route stays a one-line edit and cannot forget to register itself
// here.
type routeRecorder struct {
	mux    *http.ServeMux
	routes *[]Endpoint
}

func (r routeRecorder) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	method, path := "GET", pattern
	if i := strings.IndexByte(pattern, ' '); i > 0 {
		method, path = pattern[:i], strings.TrimSpace(pattern[i+1:])
	}
	*r.routes = append(*r.routes, Endpoint{Method: method, Path: path})
	r.mux.HandleFunc(pattern, h)
}

// HandleEndpoints lists the API surface.
func (a *API) HandleEndpoints(w http.ResponseWriter, r *http.Request) {
	out := make([]Endpoint, len(a.routes))
	copy(out, a.routes)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Count     int        `json:"count"`
		Endpoints []Endpoint `json:"endpoints"`
	}{len(out), out})
}
