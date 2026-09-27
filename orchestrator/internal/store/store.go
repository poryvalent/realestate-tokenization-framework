// Package store is the read and write path to Postgres for everything the API serves.
//
// # Why this is a separate package
//
// The domain packages are pure. offer, period, ballot, settlement and adjustment compute over values and
// touch nothing, which is why the ballot draw is reproducible from an anchored seed and why every rule in
// them can be tested without a database. Putting pgx into them to save a layer would spend that.
//
// So persistence lives here, depending on the domain rather than the other way round. A store loads rows,
// converts them into the domain's own types, and hands them over. It makes no decisions: no store in this
// package checks whether a transition is legal or whether a floor is met.
//
// # Enum columns
//
// Every enum is read with an explicit ::text cast and then converted through the domain type's own Valid
// check. A status in the database that the Go enum does not know is a hard error naming the value, not a
// zero value and not a passthrough. The alternative is serving a client a status the system cannot reason
// about, which is a quieter and more expensive failure than a 500 at the moment of the mismatch.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNotFound is returned when a row that was addressed by id does not exist.
//
// A distinct sentinel rather than pgx.ErrNoRows, because "this offer does not exist" is a fact about the
// request that the HTTP layer turns into a 404, whereas a driver error is an operational fault. Leaking
// pgx.ErrNoRows upwards would also make the HTTP layer import the driver to recognise it.
var ErrNotFound = errors.New("store: no such row")

// ErrUnknownEnumValue is returned when a column holds a value the Go enum does not define.
//
// This means a migration added a value and the Go side was not updated, or a row was written by something
// that bypassed the domain. Either way the safe answer is to refuse, because every rule keyed on that
// status would silently take the wrong branch.
var ErrUnknownEnumValue = errors.New("store: the database holds an enum value this build does not define")

// Querier is the subset of pgx a store needs.
//
// An interface rather than *pgxpool.Pool so the same store works against a transaction. A test can open a
// transaction, seed it, read through the store and roll back, which keeps tests from having to clean up
// after themselves and lets them run against a shared database without interfering.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Writer is what a write needs: a transaction.
//
// Separate from Querier on purpose. Every read in this package takes a Querier, which has no Exec, so a read
// path cannot be quietly turned into a write. A function that changes state takes a Writer, and in practice
// that is always a pgx.Tx: the business row and the outbox entry that anchors it must commit together or not
// at all, which is the entire point of a transactional outbox.
type Writer interface {
	Querier
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ErrConflict is returned when a compare-and-set write finds the row already moved.
//
// Two operators pressing the same button, or a retry racing its original, both end here. The write is refused
// rather than applied on top of a state the caller did not see.
var ErrConflict = errors.New("store: the row changed since it was read")

// unknownEnum builds the error for a value outside a Go enum.
func unknownEnum(column, value string) error {
	return fmt.Errorf("%w: %s = %q", ErrUnknownEnumValue, column, value)
}

// Pagination bounds a list query.
//
// A default and a cap are both applied, because an unbounded list is a denial of service against a scheme
// with a full register: 500 unitholders is small, but nothing stops a caller asking for every holding
// ledger row ever written.
type Pagination struct {
	// Limit is the maximum number of rows to return.
	Limit int
	// Cursor is the opaque position to resume from, empty for the first page.
	Cursor string
}

// Page size bounds, shared by every list endpoint so one cannot drift from another.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// normalise applies the default and the cap.
func (p Pagination) normalise() Pagination {
	if p.Limit <= 0 {
		p.Limit = DefaultLimit
	}
	if p.Limit > MaxLimit {
		p.Limit = MaxLimit
	}
	return p
}
