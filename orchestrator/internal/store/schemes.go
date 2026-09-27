package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/acresync/orchestrator/internal/money"
)

// Scheme is a row of the schemes table.
//
// A store-owned struct rather than a domain type because no domain type models a scheme: the scheme is the
// configuration the domain computes against, and its columns are constants of the deployment. The statutory
// figures are here as read, not recomputed, so a mismatch between the database and the contract's
// constructor arguments shows up as a difference rather than being smoothed over.
type Scheme struct {
	ID               string
	SebiSchemeRef    string
	Name             string
	IsLeveraged      bool
	AssetValue       money.Paise
	UnitPrice        money.Paise
	TotalUnits       int32
	IMUnits          int32
	PublicUnits      int32
	MinPublicHolders int32
	Status           string
	EnvironmentTag   string

	// ChainID is zero and the addresses are empty until the scheme is deployed. Nullable in the table, so
	// a scheme can be configured and reviewed before anything reaches a chain.
	ChainID       int32
	RolesAddress  string
	BallotAddress string
	SchemeAddress string
}

// Deployed reports whether the scheme has contract addresses recorded.
//
// The three addresses are written together, so any one of them being present means the deployment
// happened. Checking all three would hide a half-written row rather than reveal it, which is why the
// contracts block is served as a unit or not at all.
func (s Scheme) Deployed() bool {
	return s.SchemeAddress != "" && s.RolesAddress != "" && s.BallotAddress != ""
}

// Schemes reads the schemes table.
type Schemes struct {
	q Querier
}

// NewSchemes binds a store to a pool or transaction.
func NewSchemes(q Querier) Schemes { return Schemes{q: q} }

// schemeColumns is shared by every read so the scan order cannot drift between queries.
//
// The nullable columns are coalesced here rather than scanned into pointers, because a missing chain
// address and an empty one mean the same thing to every caller: not deployed yet.
const schemeColumns = `
	id, sebi_scheme_ref, name, is_leveraged,
	asset_value_paise, unit_price_paise,
	total_units, im_units, public_units, min_public_holders,
	status::text, environment_tag::text,
	coalesce(chain_id, 0), coalesce(roles_address, ''),
	coalesce(ballot_address, ''), coalesce(scheme_address, '')`

func scanScheme(row pgx.Row) (Scheme, error) {
	var (
		s          Scheme
		assetValue int64
		unitPrice  int64
	)

	err := row.Scan(
		&s.ID, &s.SebiSchemeRef, &s.Name, &s.IsLeveraged,
		&assetValue, &unitPrice,
		&s.TotalUnits, &s.IMUnits, &s.PublicUnits, &s.MinPublicHolders,
		&s.Status, &s.EnvironmentTag,
		&s.ChainID, &s.RolesAddress, &s.BallotAddress, &s.SchemeAddress,
	)
	if err != nil {
		return Scheme{}, err
	}

	s.AssetValue = money.Paise(assetValue)
	s.UnitPrice = money.Paise(unitPrice)
	return s, nil
}

// ByID loads one scheme.
func (s Schemes) ByID(ctx context.Context, id string) (Scheme, error) {
	const q = `SELECT ` + schemeColumns + ` FROM schemes WHERE id = $1`

	scheme, err := scanScheme(s.q.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Scheme{}, ErrNotFound
	}
	if err != nil {
		return Scheme{}, err
	}
	return scheme, nil
}

// List returns schemes ordered for stable paging.
//
// Ordered by sebi_scheme_ref, which is unique and immutable, rather than by created_at. Two schemes created
// in the same transaction share a timestamp, and a page boundary falling between them would either repeat
// or skip a row depending on how the planner happened to order that tie.
func (s Schemes) List(ctx context.Context, p Pagination) ([]Scheme, error) {
	p = p.normalise()

	const q = `
		SELECT ` + schemeColumns + `
		FROM schemes
		WHERE $1 = '' OR sebi_scheme_ref > $1
		ORDER BY sebi_scheme_ref
		LIMIT $2`

	rows, err := s.q.Query(ctx, q, p.Cursor, p.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Scheme
	for rows.Next() {
		scheme, err := scanScheme(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, scheme)
	}
	// rows.Err reports a failure that happened partway through streaming. Without this check a truncated
	// result set is indistinguishable from a short one, and a list endpoint would quietly omit rows.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
