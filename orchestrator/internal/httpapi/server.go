package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/acresync/orchestrator/internal/clock"
	"github.com/acresync/orchestrator/internal/config"
)

// Deps are what the server needs from the rest of the system.
//
// Passed in rather than constructed here so a test can supply a fake for any of them, and so this package
// never decides where data comes from. The read stores arrive as interfaces defined next to the handlers
// that use them, which keeps each handler's requirements visible at the point of use.
type Deps struct {
	// Env decides what is safe to expose. Some diagnostics belong in LOCAL and nowhere else.
	Env config.Environment

	// Wall is real elapsed time, for request logging and nothing else. Business time comes from the
	// simulated clock through the domain, never from here.
	Wall clock.Business
}

// Server holds the routed handler.
type Server struct {
	deps    Deps
	handler http.Handler
}

// New builds a server with its routes and middleware in place.
func New(deps Deps) *Server {
	if deps.Wall == nil {
		deps.Wall = clock.Real()
	}

	s := &Server{deps: deps}
	s.handler = s.routes()
	return s
}

// ServeHTTP makes the server the handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// routes declares every path and wraps them in the middleware stack.
//
// # Why the standard library's mux
//
// Since Go 1.22 ServeMux matches on method and extracts path wildcards, which is the whole of what a router
// was previously imported for. This repo has stayed deliberately thin on dependencies, and a router is a
// piece of infrastructure that sits in the request path of every single call. Not adding one is the cheaper
// choice for as long as the standard library keeps doing the job.
//
// # Why /v1 is in the pattern
//
// The prefix is part of the published contract rather than something a deployment strips, so it is visible
// here. A reverse proxy that rewrote it would make these patterns lie about the URLs they serve.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Operational endpoints, outside /v1: they describe the process, not the scheme, and a load balancer
	// should not have to know the API's version to check whether it is alive.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	// An explicit catch-all, so an unknown path produces the contract's error envelope rather than
	// ServeMux's plain-text "404 page not found". A client parsing JSON should never have to special-case
	// the one response that is not JSON.
	mux.HandleFunc("/", s.handleNotFound)

	// Ordering here is load-bearing. The request id is established first so everything inside it can log a
	// correlatable line, panic recovery sits inside that but outside the handlers so it catches them, and
	// logging wraps the handler last so it records the status the handler actually produced.
	return chain(mux,
		withRequestID,
		recoverPanics,
		logRequests(s.wallNow),
	)
}

// wallNow reads real elapsed time for logging.
//
// The clock interface returns an error because a simulated clock reads from the database. A logging path
// must not fail a request over that, so a failure degrades to a zero time and the duration is reported as
// meaningless rather than the request being refused.
func (s *Server) wallNow() time.Time {
	t, err := s.deps.Wall.Now(context.Background())
	if err != nil {
		return time.Time{}
	}
	return t
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, notFound("route "+r.Method+" "+r.URL.Path))
}

// handleHealthz reports that the process is running.
//
// Deliberately answers without touching the database. A liveness probe that fails when a dependency is down
// gets the process killed and restarted, which fixes nothing and removes the instance that could have
// served the endpoints not needing that dependency.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz reports whether the server can serve traffic.
//
// This is the one that should check dependencies. Until the stores exist there is nothing to check, so it
// answers the same as healthz and says so rather than implying a check that is not happening.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// Timeouts for the HTTP server.
//
// Set explicitly because the zero value for every one of these is "no limit", and a server with no read
// timeout will hold a connection open for a client that has stopped sending. ReadHeaderTimeout is the one
// that matters most: it is the defence against a slowloris holding sockets by trickling headers.
const (
	ReadHeaderTimeout = 5 * time.Second
	ReadTimeout       = 15 * time.Second
	WriteTimeout      = 30 * time.Second
	IdleTimeout       = 60 * time.Second
	ShutdownGrace     = 20 * time.Second
)

// HTTPServer wraps the handler in a configured http.Server.
func (s *Server) HTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s,
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
	}
}
