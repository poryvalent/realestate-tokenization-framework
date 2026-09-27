package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/acresync/orchestrator/internal/asba"
	"github.com/acresync/orchestrator/internal/clock"
	"github.com/acresync/orchestrator/internal/ipfs"
	"github.com/acresync/orchestrator/internal/ipfsguard"
	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/store"
)

// The write path.
//
// # One transaction per request
//
// Every mutation runs inside one database transaction that holds three things: the business rows, the
// chain_outbox entry that will anchor them, and the http_idempotency row that lets a retry receive the same
// answer. They commit together or not at all. That is what the transactional outbox is for: a ballot run
// written without its anchor queued, or an anchor queued for a row that rolled back, are both states the rest
// of the system has no way to recover from on its own.
//
// # What the handler does not do
//
// It never sends a transaction. It queues one, and the relayer sends it. So every endpoint that touches the
// chain answers 202 with the state as it stands, and a later step checks the queued entry's confirmation
// before depending on it. A handler that waited for a block would hold a database transaction open for
// twelve seconds on a good day, and on a bad day would hold it through a reorg.

// TxBeginner opens a transaction. *pgxpool.Pool and pgx.Tx both satisfy it; the second is how the tests run
// every mutation inside a transaction that is rolled back afterwards.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ChainReader reads what the ceremony needs from the chain.
//
// Only reads. Nothing in the HTTP layer holds a signing key or sends a transaction; that is the relayer's job
// and the reason the relayer is the sole authorised sender.
type ChainReader interface {
	// Head is the current block number.
	Head(ctx context.Context) (uint64, error)
	// BlockHash is the hash of a mined block. Zero means the chain no longer serves it.
	BlockHash(ctx context.Context, number uint64) (merkle.Hash, error)
}

// Publisher pins a document. *ipfs.Publisher satisfies it.
type Publisher interface {
	Publish(ctx context.Context, docType ipfsguard.DocType, raw []byte) (*ipfs.Pin, error)
}

// ASBAProvider is the bank side of a funds block. asba.Provider satisfies it.
type ASBAProvider = asba.Provider

// writeCtx is what a write handler receives.
type writeCtx struct {
	r         *http.Request
	tx        pgx.Tx
	body      []byte
	principal Principal
}

func (c *writeCtx) ctx() context.Context { return c.r.Context() }

// decode reads the request body strictly: unknown fields and trailing content are refused.
//
// On an endpoint that moves money, a field silently dropped is a request answered with a success describing
// something the caller did not ask for.
func (c *writeCtx) decode(dst any) error {
	if len(bytes.TrimSpace(c.body)) == 0 {
		return badRequest("a JSON body is required", nil)
	}
	dec := json.NewDecoder(bytes.NewReader(c.body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return badRequest("the request body is not valid JSON for this endpoint", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return badRequest("the request body carries content after the JSON document", nil)
	}
	return nil
}

// writeFunc is a mutation. It returns the status and the value to serve, or an error.
type writeFunc func(c *writeCtx) (int, any, error)

// principalKey scopes an idempotency key to its caller.
func principalKey(p Principal) string {
	if p.Kind == PrincipalInvestor {
		return "INVESTOR:" + p.InvestorID
	}
	return "OPERATOR:" + p.Subject
}

// replayedHeader marks a response served from http_idempotency.
const replayedHeader = "Idempotency-Replayed"

// write wraps a mutation with body limits, idempotency replay and a transaction.
//
// route is the pattern, not the path, so that one key reused against two different offers is caught by the
// stored path rather than being treated as a different route.
func (s *Server) write(route string, fn writeFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeError(w, r, &statusError{status: http.StatusRequestEntityTooLarge,
					code: CodeValidationFailed, msg: "the request body is larger than this endpoint accepts"})
				return
			}
			writeError(w, r, badRequest("the request body could not be read", nil))
			return
		}

		// Several of these endpoints take no body at all. A content type is demanded only when there is
		// something for it to describe.
		if len(bytes.TrimSpace(raw)) > 0 {
			mediaType := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
			if !strings.EqualFold(mediaType, "application/json") {
				writeError(w, r, badRequest("Content-Type must be application/json", nil))
				return
			}
		}

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

		p, ok := principalFrom(r.Context())
		if !ok {
			// These run only behind an auth guard. Reaching here without a principal is a wiring bug.
			writeError(w, r, &statusError{status: http.StatusInternalServerError, code: CodeInternal, msg: "internal error"})
			return
		}

		digest := sha256.Sum256(raw)
		scope := principalKey(p)

		// Answer from a stored response if there is one. Checked on its own short transaction, before the
		// work, so a replay does no work at all.
		if replayed, err := s.replay(w, r, scope, route, key, digest); err != nil || replayed {
			if err != nil {
				writeError(w, r, err)
			}
			return
		}

		tx, err := s.deps.DB.Begin(r.Context())
		if err != nil {
			writeError(w, r, err)
			return
		}
		// Rollback after a commit is a no-op, so this is always safe to defer.
		defer func() { _ = tx.Rollback(context.Background()) }()

		status, v, err := fn(&writeCtx{r: r, tx: tx, body: raw, principal: p})
		if err != nil {
			writeError(w, r, translatePgError(r, err))
			return
		}

		body, err := json.Marshal(v)
		if err != nil {
			writeError(w, r, err)
			return
		}

		_, err = tx.Exec(r.Context(), `
			INSERT INTO http_idempotency (principal, route, path, idempotency_key, request_sha256, status_code, response_body)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			scope, route, r.URL.Path, key, digest[:], status, body)
		if err == nil {
			err = tx.Commit(r.Context())
		}
		if err != nil {
			if isUniqueViolation(err) {
				// A concurrent request with the same key committed first. This one's work is rolled back by
				// the deferred Rollback, and the caller gets the winner's response, which is exactly what
				// they would have got by sending the request once.
				_ = tx.Rollback(context.Background())
				if replayed, rerr := s.replay(w, r, scope, route, key, digest); rerr != nil || replayed {
					if rerr != nil {
						writeError(w, r, rerr)
					}
					return
				}
			}
			writeError(w, r, translatePgError(r, err))
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
}

// replay serves a stored response, or reports a conflicting reuse of the key.
func (s *Server) replay(w http.ResponseWriter, r *http.Request, scope, route, key string, digest [32]byte) (bool, error) {
	tx, err := s.deps.DB.Begin(r.Context())
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var (
		path       string
		storedHash []byte
		status     int
		body       []byte
	)
	err = tx.QueryRow(r.Context(), `
		SELECT path, request_sha256, status_code, response_body
		  FROM http_idempotency
		 WHERE principal = $1 AND route = $2 AND idempotency_key = $3`,
		scope, route, key).Scan(&path, &storedHash, &status, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// Same key, different request. Replaying the stored response would tell the caller their second
	// request succeeded when it was never executed.
	if path != r.URL.Path || !bytes.Equal(storedHash, digest[:]) {
		return false, conflict(CodeIdempotencyConflict,
			"this Idempotency-Key was already used for a different request; send a new key for a new request")
	}

	slog.Info("replayed a stored response",
		"requestId", requestIDFrom(r.Context()), "route", route, "status", status)

	w.Header().Set(replayedHeader, "true")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	return true, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// translatePgError turns a database refusal into a response the caller can act on.
//
// The schema enforces a good deal on its own: bid bounds by trigger, the reveal order by trigger, dozens of
// CHECK constraints. When one fires it is a refusal, not a fault, and a 500 would tell the caller nothing.
//
// The triggers raise sentences written for people ("bid of 30 units exceeds the offer maximum of 25"), so
// those are passed through. A bare CHECK violation names a relation and a constraint instead, which is
// schema detail rather than an explanation, so it is logged and answered generically.
func translatePgError(r *http.Request, err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}

	switch pgErr.Code {
	case "23514": // check_violation, raised by both CHECK constraints and our triggers
		if strings.HasPrefix(pgErr.Message, "new row for relation") {
			slog.Info("a check constraint refused a write",
				"requestId", requestIDFrom(r.Context()), "constraint", pgErr.ConstraintName)
			return &statusError{status: http.StatusUnprocessableEntity, code: CodeValidationFailed,
				msg: "the request would violate a rule the database enforces"}
		}
		return &statusError{status: http.StatusUnprocessableEntity, code: CodeValidationFailed, msg: pgErr.Message}

	case "23505": // unique_violation
		slog.Info("a unique constraint refused a write",
			"requestId", requestIDFrom(r.Context()), "constraint", pgErr.ConstraintName)
		return conflict(CodePreconditionFailed, "this already exists")
	}
	return err
}

// nowFor reads a scheme's business time.
//
// Business time, not the wall clock: an offer opens when the simulated calendar says it does, which is what
// lets a distribution run be rehearsed end to end in an afternoon. A scheme with no clock is refused with an
// explanation rather than falling back to the wall clock, which would quietly put one scheme on a different
// timeline from every other part of the system.
func (s *Server) nowFor(ctx context.Context, schemeID string) (time.Time, error) {
	if s.deps.BusinessClock == nil {
		return time.Time{}, errors.New("httpapi: no business clock is configured")
	}
	now, err := s.deps.BusinessClock(schemeID).Now(ctx)
	if errors.Is(err, clock.ErrNotInitialised) {
		return time.Time{}, conflict(CodePreconditionFailed,
			"this scheme has no business clock yet; initialise it before acting on the scheme's timeline")
	}
	if err != nil {
		return time.Time{}, err
	}
	return now.UTC(), nil
}

// requireDeployed refuses a chain-touching action on a scheme with no contracts.
func requireDeployed(sc store.Scheme) error {
	if !sc.Deployed() {
		return conflict(CodePreconditionFailed,
			"this scheme has no deployed contracts, so there is nowhere to anchor this; deploy before acting")
	}
	return nil
}

// adminAction is one row of the operator audit trail.
type adminAction struct {
	Action     string
	SchemeID   string
	TargetType string
	TargetID   string
	Params     map[string]any
	Key        []byte
	OutboxID   string
}

// recordAdminAction writes the audit row for an operator mutation, in the same transaction as the mutation.
//
// admin_actions is append-only by trigger. Recording it in the same transaction means there is no operator
// action that happened without a record and no record of an action that rolled back.
func recordAdminAction(c *writeCtx, env string, simNow timestamp, a adminAction) error {
	params, err := json.Marshal(a.Params)
	if err != nil {
		return err
	}
	if a.Params == nil {
		params = []byte("{}")
	}

	var targetID, outboxID any
	if a.TargetID != "" {
		targetID = a.TargetID
	}
	if a.OutboxID != "" {
		outboxID = a.OutboxID
	}
	var targetType any
	if a.TargetType != "" {
		targetType = a.TargetType
	}

	_, err = c.tx.Exec(c.ctx(), `
		INSERT INTO admin_actions (
			actor_user_id, action, scheme_id, target_entity_type, target_entity_id,
			simulated_clock_value, params_json, idempotency_key, environment_tag, resulting_outbox_id
		) VALUES ($1, $2::admin_action, $3, $4, $5, $6, $7::jsonb, $8, $9::environment_tag, $10)`,
		c.principal.Subject, a.Action, a.SchemeID, targetType, targetID,
		simNow, string(params), a.Key, env, outboxID)
	if err != nil {
		return fmt.Errorf("recording the %s audit entry: %w", a.Action, err)
	}
	return nil
}
