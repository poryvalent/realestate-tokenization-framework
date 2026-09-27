// Package httpapi serves the contract published at docs/api/openapi.yaml.
//
// # Layering
//
// Handlers translate HTTP to domain calls and back. They hold no business rules: every decision about
// what is permitted belongs to a domain package, which is why those packages are pure functions and stay
// that way. Nothing here imports pgx either; persistence lives in internal/store.
//
// That ordering matters more than it looks. A rule enforced in a handler is a rule enforced only for
// callers who arrive over HTTP, and the relayer, the migrations and the e2e suite do not.
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/acresync/orchestrator/internal/adjustment"
	"github.com/acresync/orchestrator/internal/bidbook"
	"github.com/acresync/orchestrator/internal/offer"
	"github.com/acresync/orchestrator/internal/period"
	"github.com/acresync/orchestrator/internal/settlement"
	"github.com/acresync/orchestrator/internal/store"
)

// Error codes, matching the enum in the published contract exactly.
const (
	CodePreconditionFailed   = "precondition_failed"
	CodeNotFeasible          = "not_feasible"
	CodeAnchorNotConfirmed   = "anchor_not_confirmed"
	CodeCeremonyOrder        = "ceremony_order"
	CodeUnitsIssued          = "units_issued"
	CodeSettlementIncomplete = "settlement_incomplete"
	CodeValidationFailed     = "validation_failed"
	CodeIdempotencyConflict  = "idempotency_conflict"
	CodePaused               = "paused"
	CodeNotFound             = "not_found"
	CodeUnauthorized         = "unauthorized"
	CodeForbidden            = "forbidden"
	CodeInternal             = "internal"
)

// apiError is the wire shape of the contract's error envelope.
type apiError struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Details   map[string]any `json:"details,omitempty"`
	RequestID string         `json:"requestId,omitempty"`
}

// statusError is an error carrying an explicit HTTP status and code.
//
// Used where the cause is the request itself rather than a domain rule: a malformed identifier, a missing
// header, an unknown route. Domain failures are never wrapped in these, because the table below already
// knows what they mean.
type statusError struct {
	status int
	code   string
	msg    string
	err    error
}

func (e *statusError) Error() string {
	if e.err != nil {
		return e.msg + ": " + e.err.Error()
	}
	return e.msg
}

func (e *statusError) Unwrap() error { return e.err }

func notFound(what string) error {
	return &statusError{status: http.StatusNotFound, code: CodeNotFound, msg: "no such " + what}
}

func badRequest(msg string, err error) error {
	return &statusError{status: http.StatusBadRequest, code: CodeValidationFailed, msg: msg, err: err}
}

func unauthorized(msg string) error {
	return &statusError{status: http.StatusUnauthorized, code: CodeUnauthorized, msg: msg}
}

func forbidden(msg string) error {
	return &statusError{status: http.StatusForbidden, code: CodeForbidden, msg: msg}
}

func conflict(code, msg string) error {
	return &statusError{status: http.StatusConflict, code: code, msg: msg}
}

// classification is one row of the sentinel-to-HTTP mapping.
type classification struct {
	err    error
	status int
	code   string
}

// errorClasses maps every domain sentinel to a status and a contract error code.
//
// # Why this is a table and not a switch
//
// A switch cannot be enumerated, and an unmapped sentinel falls through to a 500 that says nothing. That
// failure is invisible: the sentinel is reachable, the handler looks correct, and the defect only shows up
// as somebody being told "internal error" when they sent a bad enum value. The table is data, so
// TestEveryDomainSentinelIsClassified can read the domain packages' source, collect every sentinel, and
// fail if one is missing from here. Adding an error to a domain package now breaks this package's tests
// until somebody decides what it means over HTTP.
//
// # Order
//
// First match wins, so the specific codes are listed before the general ones. An error that wraps both
// ErrPreconditionFailed and something more precise should be reported as the more precise thing.
//
// # Why the domain's own message is passed through
//
// These sentinels carry sentences that state what is wrong and often why the rule exists, such as "fiat
// has settled; use a carry-forward adjustment". Replacing that with a generic "conflict" throws away the
// most useful thing the system knows at the moment somebody most needs it. The code is what a client
// branches on; the message is the domain's, verbatim.
var errorClasses = []classification{
	// A row that does not exist. Mapped here rather than translated in each handler, so a handler that
	// forgets to check cannot turn a missing offer into a 500.
	{store.ErrNotFound, http.StatusNotFound, CodeNotFound},

	// A verified login that holds nothing here. Forbidden rather than not-found: the caller and their
	// credential are genuine, and what is absent is a relationship with the register. A 404 would suggest the
	// endpoint is missing and a 401 would send them round a login loop that cannot succeed.
	{store.ErrNoInvestorForIdentity, http.StatusForbidden, CodeForbidden},

	// A pause applies across every domain and is the most specific thing that can be said, so it is first.
	{offer.ErrPaused, http.StatusLocked, CodePaused},
	{period.ErrPaused, http.StatusLocked, CodePaused},

	// The statutory floors. A client branches on these identically wherever they are raised, so the
	// settlement-time holder floor gets the same code as the offer-time feasibility check rather than
	// being buried in a generic validation failure.
	{offer.ErrNotFeasible, http.StatusConflict, CodeNotFeasible},
	{settlement.ErrHolderFloor, http.StatusConflict, CodeNotFeasible},

	{offer.ErrAnchorNotConfirmed, http.StatusConflict, CodeAnchorNotConfirmed},
	{period.ErrAnchorNotConfirmed, http.StatusConflict, CodeAnchorNotConfirmed},

	{offer.ErrCeremonyOrder, http.StatusConflict, CodeCeremonyOrder},
	{offer.ErrUnitsIssued, http.StatusConflict, CodeUnitsIssued},
	{offer.ErrSettlementIncomplete, http.StatusConflict, CodeSettlementIncomplete},

	// Rejections of supplied data: the request is well-formed but describes something the domain will not
	// accept. 422 rather than 400, because parsing succeeded and the objection is semantic.
	{offer.ErrUnknownStatus, http.StatusUnprocessableEntity, CodeValidationFailed},
	{period.ErrUnknownStatus, http.StatusUnprocessableEntity, CodeValidationFailed},
	{period.ErrUnknownReason, http.StatusUnprocessableEntity, CodeValidationFailed},
	{adjustment.ErrUnknownDirection, http.StatusUnprocessableEntity, CodeValidationFailed},

	{settlement.ErrDuplicateWallet, http.StatusUnprocessableEntity, CodeValidationFailed},
	{settlement.ErrZeroWallet, http.StatusUnprocessableEntity, CodeValidationFailed},
	{settlement.ErrBadWallet, http.StatusUnprocessableEntity, CodeValidationFailed},
	{settlement.ErrZeroUnits, http.StatusUnprocessableEntity, CodeValidationFailed},
	{settlement.ErrUnitsMismatch, http.StatusUnprocessableEntity, CodeValidationFailed},
	{settlement.ErrHoldersMismatch, http.StatusUnprocessableEntity, CodeValidationFailed},
	{settlement.ErrTotalUnitsMismatch, http.StatusUnprocessableEntity, CodeValidationFailed},
	{settlement.ErrCursorMismatch, http.StatusUnprocessableEntity, CodeValidationFailed},
	{settlement.ErrBatchTooLarge, http.StatusUnprocessableEntity, CodeValidationFailed},
	{settlement.ErrNoAllottees, http.StatusUnprocessableEntity, CodeValidationFailed},

	{bidbook.ErrUnfunded, http.StatusUnprocessableEntity, CodeValidationFailed},
	{bidbook.ErrAmountMismatch, http.StatusUnprocessableEntity, CodeValidationFailed},
	{bidbook.ErrDuplicateInvestor, http.StatusUnprocessableEntity, CodeValidationFailed},
	{bidbook.ErrDuplicateBidRef, http.StatusUnprocessableEntity, CodeValidationFailed},
	{bidbook.ErrDuplicateAnchor, http.StatusUnprocessableEntity, CodeValidationFailed},
	{bidbook.ErrCountsDisagree, http.StatusUnprocessableEntity, CodeValidationFailed},
	{bidbook.ErrTotalOverflow, http.StatusUnprocessableEntity, CodeValidationFailed},
	{bidbook.ErrPinDoesNotBind, http.StatusUnprocessableEntity, CodeValidationFailed},
	{bidbook.ErrWrongDocType, http.StatusUnprocessableEntity, CodeValidationFailed},

	{adjustment.ErrNothingToAdjust, http.StatusUnprocessableEntity, CodeValidationFailed},
	{adjustment.ErrTargetNotLater, http.StatusUnprocessableEntity, CodeValidationFailed},
	{adjustment.ErrSamePeriod, http.StatusUnprocessableEntity, CodeValidationFailed},
	{adjustment.ErrNoNarrative, http.StatusUnprocessableEntity, CodeValidationFailed},

	{period.ErrNoNarrative, http.StatusUnprocessableEntity, CodeValidationFailed},
	{period.ErrUnexplained, http.StatusUnprocessableEntity, CodeValidationFailed},
	{period.ErrDiffUnaccounted, http.StatusUnprocessableEntity, CodeValidationFailed},
	{period.ErrDiffNotInRun, http.StatusUnprocessableEntity, CodeValidationFailed},
	{period.ErrProofMismatched, http.StatusUnprocessableEntity, CodeValidationFailed},

	// State refusals: the act is legitimate but not from here, or not yet, or not any more. These are the
	// ones where the message matters most, because it usually names the remedy.
	{period.ErrFiatSettled, http.StatusConflict, CodePreconditionFailed},
	{period.ErrDiverged, http.StatusConflict, CodePreconditionFailed},
	{period.ErrNotDiverged, http.StatusConflict, CodePreconditionFailed},
	{period.ErrStillDiverged, http.StatusConflict, CodePreconditionFailed},
	{period.ErrFourEyes, http.StatusConflict, CodePreconditionFailed},
	{period.ErrNotFourEyes, http.StatusConflict, CodePreconditionFailed},
	{period.ErrApprovalMismatch, http.StatusConflict, CodePreconditionFailed},
	{period.ErrPeriodNotReversible, http.StatusConflict, CodePreconditionFailed},
	{period.ErrPayoutsInFlight, http.StatusConflict, CodePreconditionFailed},
	{period.ErrEntitlementsPartial, http.StatusConflict, CodePreconditionFailed},

	{settlement.ErrNotBoundToBallot, http.StatusConflict, CodePreconditionFailed},
	{settlement.ErrIMNotRecorded, http.StatusConflict, CodePreconditionFailed},

	{bidbook.ErrEmptyBook, http.StatusConflict, CodePreconditionFailed},
	{bidbook.ErrNoPin, http.StatusConflict, CodePreconditionFailed},

	{adjustment.ErrNotOpen, http.StatusConflict, CodePreconditionFailed},
	{adjustment.ErrNoTarget, http.StatusConflict, CodePreconditionFailed},
	{adjustment.ErrNotRecoverable, http.StatusConflict, CodePreconditionFailed},

	{offer.ErrBookNotFixed, http.StatusConflict, CodePreconditionFailed},

	// The catch-alls last, so anything more specific above wins.
	{offer.ErrIllegalTransition, http.StatusConflict, CodePreconditionFailed},
	{period.ErrIllegalTransition, http.StatusConflict, CodePreconditionFailed},
	{offer.ErrPreconditionFailed, http.StatusConflict, CodePreconditionFailed},
	{period.ErrPreconditionFailed, http.StatusConflict, CodePreconditionFailed},
}

// classify maps an error to an HTTP status and a contract error code.
//
// An unrecognised error becomes a bare 500 whose message is "internal error" and nothing else. An error
// that has not been considered might carry a connection string or a row of somebody's data, and the
// guard test exists so that no domain sentinel ever reaches this fallback.
func classify(err error) (status int, code, message string) {
	var se *statusError
	if errors.As(err, &se) {
		return se.status, se.code, se.Error()
	}

	for _, c := range errorClasses {
		if errors.Is(err, c.err) {
			return c.status, c.code, err.Error()
		}
	}

	return http.StatusInternalServerError, CodeInternal, "internal error"
}

// writeError renders an error using the contract's envelope.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, msg := classify(err)
	reqID := requestIDFrom(r.Context())

	// The full error is logged even when a redacted message is returned, so a 500 stays diagnosable
	// without the response carrying whatever the error happened to hold.
	if status >= http.StatusInternalServerError {
		slog.Error("request failed",
			"requestId", reqID, "method", r.Method, "path", r.URL.Path, "err", err)
	} else {
		slog.Info("request refused",
			"requestId", reqID, "method", r.Method, "path", r.URL.Path,
			"status", status, "code", code)
	}

	writeJSON(w, r, status, apiError{Error: apiErrorBody{
		Code:      code,
		Message:   msg,
		RequestID: reqID,
	}})
}

// marshalForETag encodes a value for both the body and its ETag.
//
// encoding/json is deterministic for a given value: struct fields follow declaration order and map keys are
// sorted. That matters because the ETag is a hash of these exact bytes, so a non-deterministic encoder
// would produce a different tag for an unchanged representation and defeat every conditional request.
func marshalForETag(v any) ([]byte, error) {
	return json.Marshal(v)
}

// writeJSON renders a value, or logs if it cannot be encoded.
//
// Encoding into a buffer first rather than straight to the ResponseWriter, because a marshalling failure
// halfway through a direct write leaves a 200 with a truncated body, which a client cannot distinguish
// from a network truncation.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("encoding a response failed",
			"requestId", requestIDFrom(r.Context()), "path", r.URL.Path, "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"internal error"}}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	// A HEAD request must carry the headers and no body.
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}
