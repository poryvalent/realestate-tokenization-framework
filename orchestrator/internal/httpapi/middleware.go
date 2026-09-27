package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxPrincipal
	ctxIdempotencyKey
)

// requestIDFrom returns the request's identifier, or empty if unset.
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

// withRequestID attaches an identifier to every request and echoes it.
//
// An inbound X-Request-Id is honoured so a trace can be followed across a proxy, but it is not trusted as
// a unique key for anything: a client controls it and could repeat it. It exists to correlate logs, and the
// idempotency key is the thing that decides whether work happens twice.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" || len(id) > 128 {
			var b [8]byte
			if _, err := rand.Read(b[:]); err != nil {
				// A failure here is not worth refusing a request over; correlation degrades, nothing else.
				id = "req_unknown"
			} else {
				id = "req_" + hex.EncodeToString(b[:])
			}
		}

		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

// recoverPanics turns a panic into a 500 rather than a dropped connection.
//
// A panicking handler otherwise kills the connection with no response, which a client sees as a network
// fault and may retry. On a mutation that is the worst possible reading of the failure.
func recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.Error("handler panicked",
					"requestId", requestIDFrom(r.Context()),
					"method", r.Method, "path", r.URL.Path, "panic", v)

				writeJSON(w, r, http.StatusInternalServerError, apiError{Error: apiErrorBody{
					Code: CodeInternal, Message: "internal error",
					RequestID: requestIDFrom(r.Context()),
				}})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status code for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// logRequests records one line per request.
//
// Elapsed time is measured with the real clock rather than business time, which the wall-clock checker
// permits through clock.Real: how long a request took is transport metadata and has nothing to do with the
// simulated timeline a distribution runs on.
func logRequests(now func() time.Time) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := now()
			rec := &statusRecorder{ResponseWriter: w}

			next.ServeHTTP(rec, r)

			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			slog.Info("request",
				"requestId", requestIDFrom(r.Context()),
				"method", r.Method, "path", r.URL.Path,
				"status", rec.status, "bytes", rec.bytes,
				"ms", now().Sub(start).Milliseconds())
		})
	}
}

// etagFor derives a strong ETag from a response body.
//
// SHA-256 of the bytes, so it changes exactly when the representation does. Deriving it from the content
// rather than from an updated_at column is deliberate: a timestamp can move without the representation
// changing, which sends a poller a fresh body for no reason, and it can fail to move when a related row
// changes, which is worse because the poller never sees the update.
func etagFor(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// writeJSONWithETag renders a value and handles conditional requests.
//
// This is what makes a three-second poll cheap. The contract documents that a 304 requires If-None-Match,
// and a caller who omits it simply gets the body every time.
func writeJSONWithETag(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := marshalForETag(v)
	if err != nil {
		writeError(w, r, err)
		return
	}

	tag := etagFor(body)
	w.Header().Set("ETag", tag)

	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, tag) {
		// 304 must carry no body. The ETag is repeated so a client that discarded it can resynchronise.
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// etagMatches reports whether an If-None-Match header covers the given tag.
//
// Handles the wildcard and a comma-separated list, because both are legal and a client library may send
// either. Weak comparison is not implemented: every tag here is strong, and treating a weak match as a hit
// would risk answering 304 to a request whose representation had genuinely changed.
func etagMatches(header, tag string) bool {
	if header == "*" {
		return true
	}
	for len(header) > 0 {
		var candidate string
		if i := indexByte(header, ','); i >= 0 {
			candidate, header = header[:i], header[i+1:]
		} else {
			candidate, header = header, ""
		}
		if trimSpace(candidate) == tag {
			return true
		}
	}
	return false
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// chain applies middleware so the first listed runs outermost.
//
// Ordering is load-bearing. The request id has to be established before anything that logs, and the panic
// recovery has to sit inside the request id so its log line can be correlated, but outside the handler so
// it actually catches anything.
func chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}
