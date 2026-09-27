package httpapi

import (
	"context"
	"net/http"
	"strings"
)

// The Idempotency-Key header, and how it relates to the key the system actually relies on.
//
// # Two different keys, protecting two different things
//
// internal/idempotency derives a key from the business content of an action:
//
//	sha256(frame(domain) || frame(action) || frame(schemeID) || frame(scopeID) || frame(canonicalPayload))
//
// That derived key is what every table's UNIQUE idempotency_key holds and what the contract's usedKeys
// mapping records. It is the real protection, and its important property is that it does not need the client
// to cooperate: a double-clicked button or a retried request after a timeout produces the same key from the
// same content, so "inject this month's rent" cannot happen twice even if the caller invents a fresh UUID
// every attempt.
//
// The header is a different thing. It is the client's own handle on its request, and the contract requires it
// on all thirteen mutations. It does two jobs:
//
//   - It makes the client state up front that it has thought about retries. An endpoint that accepts a
//     mutation without one silently invites the caller to discover the problem after a double submit.
//   - It gives the client a stable handle for asking again about the same request, which is what turns a
//     timeout into a question rather than a gamble.
//
// What the header must NOT become is the thing the system trusts. Were the derived key replaced by whatever
// the caller sent, a client that forgot to reuse its key on retry would double-spend, and the protection
// would be exactly as good as the least careful caller. So the header is admitted, validated and recorded,
// and the domain still derives its own key from content.
//
// # What is not here yet
//
// Replaying a stored HTTP response for a repeated header is not implemented, because there are no mutations
// yet to replay. Doing it properly needs somewhere to keep the first response, and that decision belongs with
// the first endpoint that needs it rather than being guessed at now. Until then a retry re-reaches the
// domain, where the derived key makes the second attempt a no-op rather than a second execution, which is the
// property that matters.

// Idempotency-Key bounds.
//
// A minimum because a single character is not a key and accepting it would make the requirement decorative. A
// maximum because the value is logged and indexed, and an unbounded header is a cheap way to inflate both.
const (
	idempotencyKeyHeader = "Idempotency-Key"
	minIdempotencyKeyLen = 8
	maxIdempotencyKeyLen = 255
)

// idempotencyKeyFrom returns the caller's key, and whether one was supplied.
func idempotencyKeyFrom(ctx context.Context) (string, bool) {
	key, ok := ctx.Value(ctxIdempotencyKey).(string)
	return key, ok
}

// requireIdempotencyKey admits a mutation only with a usable key.
//
// Applied per route rather than to every request, because it is only meaningful on a mutation. A blanket
// check would demand the header on reads, where retrying is already free and the requirement would be noise.
func requireIdempotencyKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(idempotencyKeyHeader)
		if key == "" {
			writeError(w, r, badRequest(
				idempotencyKeyHeader+" is required on this request, so that a retry cannot become a second action", nil))
			return
		}
		if err := validateIdempotencyKey(key); err != nil {
			writeError(w, r, err)
			return
		}

		next(w, r.WithContext(context.WithValue(r.Context(), ctxIdempotencyKey, key)))
	}
}

// validateIdempotencyKey checks length and charset.
//
// The charset is restricted to printable ASCII without spaces or control characters. This is not about
// injection into SQL, which parameterised queries already handle, but about what ends up in a log line: a
// value carrying a newline can forge a second log entry, and a log that can be forged by a request header is
// not evidence of anything.
func validateIdempotencyKey(key string) error {
	if len(key) < minIdempotencyKeyLen {
		return badRequest(idempotencyKeyHeader+" is too short to be a meaningful key", nil)
	}
	if len(key) > maxIdempotencyKeyLen {
		return badRequest(idempotencyKeyHeader+" is longer than 255 characters", nil)
	}

	for i := 0; i < len(key); i++ {
		c := key[i]
		if c <= 0x20 || c >= 0x7f {
			return badRequest(
				idempotencyKeyHeader+" must be printable ASCII with no spaces or control characters", nil)
		}
	}
	return nil
}

// requireJSONBody rejects a mutation whose body is not declared as JSON.
//
// Checked because every mutation in the contract takes a JSON body, and a request arriving as
// form-urlencoded is a client that has misunderstood the API. Failing at admission names that, rather than
// letting the decode fail later with a message about unexpected characters.
func requireJSONBody(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		contentType := r.Header.Get("Content-Type")
		if contentType == "" {
			writeError(w, r, badRequest("Content-Type is required and must be application/json", nil))
			return
		}

		// The header may carry parameters, as in "application/json; charset=utf-8", which is legal.
		mediaType := strings.TrimSpace(strings.Split(contentType, ";")[0])
		if !strings.EqualFold(mediaType, "application/json") {
			writeError(w, r, badRequest("Content-Type must be application/json, got "+mediaType, nil))
			return
		}

		next(w, r)
	}
}

// maxBodyBytes bounds a request body.
//
// Every mutation in this API is a small JSON document; the largest is a batch of settlement entries capped at
// a hundred. Without a limit, a single request can make the process allocate until it dies, and a decoder
// reading from the network will happily do that.
const maxBodyBytes = 1 << 20 // 1 MiB

// limitBody caps how much of a request body will be read.
func limitBody(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next(w, r)
	}
}

// mutation composes the checks every state-changing endpoint needs.
//
// One helper rather than a list repeated per route, because the failure mode of repeating it is a route that
// quietly omits one. The order matters: the body is capped before anything reads it, the content type is
// checked before a decode is attempted, and the idempotency key is demanded before the handler can act.
func mutation(next http.HandlerFunc) http.HandlerFunc {
	return limitBody(requireJSONBody(requireIdempotencyKey(next)))
}
