package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/config"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return New(Deps{Env: config.EnvLocal})
}

// decodeError reads the contract's error envelope from a response.
func decodeError(t *testing.T, res *http.Response) apiError {
	t.Helper()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	var env apiError
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("the response was not the contract's error envelope: %v\nbody: %s", err, body)
	}
	return env
}

// TestUnknownRouteReturnsTheErrorEnvelope covers the reason for the catch-all route.
//
// ServeMux's own 404 is plain text. A client written against the contract parses JSON on every failure, so
// the one response that is not JSON is the one that crashes its error handling.
func TestUnknownRouteReturnsTheErrorEnvelope(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()

	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/nothing-here", nil))

	res := rec.Result()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	env := decodeError(t, res)
	if env.Error.Code != CodeNotFound {
		t.Fatalf("code = %q, want %q", env.Error.Code, CodeNotFound)
	}
	if env.Error.RequestID == "" {
		t.Fatal("the envelope must carry the request id, so a report can be correlated with the logs")
	}
	if !strings.Contains(env.Error.Message, "/v1/nothing-here") {
		t.Fatalf("the message should name the route that was missed, got %q", env.Error.Message)
	}
}

// TestHealthzDoesNotDependOnAnything states the liveness contract.
func TestHealthzAnswersWithoutDependencies(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()

	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestMethodMismatchIsNotASilentSuccess checks the mux's method matching.
func TestMethodMismatchIsRejected(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()

	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))

	// The catch-all claims "/" for any method, so a POST to a GET-only route lands there as a 404 rather
	// than a 405. Either is defensible; what matters is that it is not a 200 and it is JSON.
	if rec.Code == http.StatusOK {
		t.Fatal("a POST to a GET-only route must not succeed")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json even for a rejection", ct)
	}
}

// TestPanicBecomesA500 is the reason recoverPanics exists.
//
// Without it a panicking handler closes the connection with no response. A client sees a network fault, and
// on a mutation the safest reading of a network fault is "retry", which is exactly wrong.
func TestPanicBecomesA500WithAnEnvelope(t *testing.T) {
	handler := chain(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("the database connection exploded with password=hunter2")
		}),
		withRequestID, recoverPanics,
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/schemes", nil))

	res := rec.Result()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.StatusCode)
	}

	env := decodeError(t, res)
	if env.Error.Code != CodeInternal {
		t.Fatalf("code = %q, want %q", env.Error.Code, CodeInternal)
	}
	if strings.Contains(env.Error.Message, "hunter2") {
		t.Fatalf("the panic value leaked into the response: %q", env.Error.Message)
	}
	if env.Error.RequestID == "" {
		t.Fatal("even a panic must carry the request id; it is the only way to find the log line")
	}
}

// TestRequestIDIsGeneratedAndEchoed covers the default path.
func TestRequestIDIsGeneratedAndEchoed(t *testing.T) {
	var seen string
	handler := withRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = requestIDFrom(r.Context())
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Fatal("the handler saw no request id")
	}
	if got := rec.Header().Get("X-Request-Id"); got != seen {
		t.Fatalf("the echoed id %q differs from the one the handler saw %q", got, seen)
	}
	if !strings.HasPrefix(seen, "req_") {
		t.Fatalf("a generated id should be recognisable as generated, got %q", seen)
	}
}

// TestInboundRequestIDIsHonoured lets a trace span a proxy.
func TestInboundRequestIDIsHonoured(t *testing.T) {
	var seen string
	handler := withRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = requestIDFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "trace-from-the-edge")

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if seen != "trace-from-the-edge" {
		t.Fatalf("request id = %q, want the inbound value", seen)
	}
}

// TestOversizedInboundRequestIDIsReplaced stops a client dictating unbounded log content.
//
// The id goes into every log line for the request. A caller who can make it arbitrarily long can inflate the
// logs, so it is accepted only within a bound.
func TestOversizedInboundRequestIDIsReplaced(t *testing.T) {
	var seen string
	handler := withRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = requestIDFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", strings.Repeat("a", 129))

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if len(seen) > 128 {
		t.Fatalf("an oversized id was accepted, length %d", len(seen))
	}
	if !strings.HasPrefix(seen, "req_") {
		t.Fatalf("want a generated replacement, got %q", seen)
	}
}

// TestGeneratedRequestIDsDiffer checks the ids are actually distinguishing anything.
func TestGeneratedRequestIDsDiffer(t *testing.T) {
	handler := withRequestID(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		id := rec.Header().Get("X-Request-Id")
		if seen[id] {
			t.Fatalf("request id %q was issued twice", id)
		}
		seen[id] = true
	}
}

// TestWallNowDegradesRatherThanFailing covers the logging clock.
//
// A clock that errors must not turn into a failed request: the duration is metadata.
func TestWallNowDegradesRatherThanFailing(t *testing.T) {
	s := New(Deps{Env: config.EnvLocal, Wall: failingClock{}})

	if got := s.wallNow(); !got.IsZero() {
		t.Fatalf("a failing clock should read as the zero time, got %v", got)
	}

	// And the server still serves.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("a failing logging clock broke the request: status %d", rec.Code)
	}
}

// failingClock stands in for a simulated clock whose database read fails.
type failingClock struct{}

func (failingClock) Now(context.Context) (time.Time, error) {
	return time.Time{}, errors.New("reading the simulated clock: connection refused")
}

// TestServerTimeoutsAreSet guards against the zero value, which means "no limit".
func TestServerTimeoutsAreSet(t *testing.T) {
	srv := newTestServer(t).HTTPServer("127.0.0.1:0")

	if srv.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout is unset, so a client trickling headers can hold a connection open")
	}
	if srv.ReadTimeout == 0 {
		t.Error("ReadTimeout is unset")
	}
	if srv.WriteTimeout == 0 {
		t.Error("WriteTimeout is unset")
	}
	if srv.IdleTimeout == 0 {
		t.Error("IdleTimeout is unset")
	}
	if srv.Handler == nil {
		t.Error("the server has no handler")
	}
}
