package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/period"
)

// Period is a row of the distribution_periods table.
type Period struct {
	ID          string
	SchemeID    string
	PeriodSeq   int32
	PeriodLabel string
	PeriodStart time.Time
	PeriodEnd   time.Time

	// RecordDate is nil until the record date is declared.
	RecordDate *time.Time

	Status period.Status

	// The distribution figures. Nil until the NDCF statement is drafted, and guaranteed present together
	// from ANCHORED onwards by periods_anchored_requires_figures.
	NdcfPaise        *money.Paise
	DistributedPaise *money.Paise
	DistributionBps  *int32

	// StatementSHA256 is the digest of the NDCF statement, empty until drafted.
	StatementSHA256 []byte
	StatementURI    string
	NdcfIPFSCid     string

	// The four-eyes record. Both approvals are required from ANCHORED onwards, and the approvers must be
	// different people, enforced by periods_approvers_distinct.
	IMApprovedBy      string
	IMApprovedAt      *time.Time
	TrusteeApprovedBy string
	TrusteeApprovedAt *time.Time

	AnchoredTx string

	// The reversal record. A reversed period stays visible forever by design: an audit trail that can hide
	// its own corrections is worth nothing.
	ReversalReason    string
	ReversalNarrative []byte
	ReversedAt        *time.Time

	ClosedAt  *time.Time
	CreatedAt time.Time
}

// Snapshot is a row of register_snapshots.
//
// Loaded separately from the period because the contract marks it present only once the register is frozen,
// and because it is the expensive half: a period read is one row, whereas a snapshot read is a row plus, for
// anything that needs lines, a scan of the register.
type Snapshot struct {
	ID              string
	PeriodID        string
	RecordDate      time.Time
	TakenAt         time.Time
	TotalUnits      int32
	DistinctHolders int32
	MerkleRoot      []byte
	CIDDigest       []byte
	IPFSCid         string
	AnchorStatus    string
	AnchoredTx      string

	// LineCount is the number of register lines, which exceeds DistinctHolders by the excluded manager.
	// Counted rather than stored, because a stored count is a second source of truth for something the
	// lines themselves already answer.
	LineCount int64
}

// Periods reads the distribution_periods table.
type Periods struct {
	q Querier
}

// NewPeriods binds a store to a pool or transaction.
func NewPeriods(q Querier) Periods { return Periods{q: q} }

const periodColumns = `
	id, scheme_id, period_seq, period_label,
	period_start, period_end, record_date,
	status::text,
	ndcf_paise, distributed_paise, distribution_bps,
	ndcf_statement_sha256, coalesce(ndcf_statement_uri, ''), coalesce(ndcf_ipfs_cid, ''),
	coalesce(im_approved_by, ''), im_approved_at,
	coalesce(trustee_approved_by, ''), trustee_approved_at,
	coalesce(anchored_tx, ''),
	coalesce(reversal_reason::text, ''), reversal_narrative_sha256, reversed_at,
	closed_at, created_at`

func scanPeriod(row pgx.Row) (Period, error) {
	var (
		p           Period
		statusText  string
		ndcf        *int64
		distributed *int64
	)

	err := row.Scan(
		&p.ID, &p.SchemeID, &p.PeriodSeq, &p.PeriodLabel,
		&p.PeriodStart, &p.PeriodEnd, &p.RecordDate,
		&statusText,
		&ndcf, &distributed, &p.DistributionBps,
		&p.StatementSHA256, &p.StatementURI, &p.NdcfIPFSCid,
		&p.IMApprovedBy, &p.IMApprovedAt,
		&p.TrusteeApprovedBy, &p.TrusteeApprovedAt,
		&p.AnchoredTx,
		&p.ReversalReason, &p.ReversalNarrative, &p.ReversedAt,
		&p.ClosedAt, &p.CreatedAt,
	)
	if err != nil {
		return Period{}, err
	}

	p.Status = period.Status(statusText)
	if !p.Status.Valid() {
		return Period{}, unknownEnum("distribution_periods.status", statusText)
	}

	// Money is converted rather than scanned directly, so the paise type is applied in exactly one place
	// and a raw int64 cannot reach a caller that would treat it as rupees.
	if ndcf != nil {
		v := money.Paise(*ndcf)
		p.NdcfPaise = &v
	}
	if distributed != nil {
		v := money.Paise(*distributed)
		p.DistributedPaise = &v
	}

	p.PeriodStart = p.PeriodStart.UTC()
	p.PeriodEnd = p.PeriodEnd.UTC()
	p.CreatedAt = p.CreatedAt.UTC()
	p.RecordDate = utcPtr(p.RecordDate)
	p.IMApprovedAt = utcPtr(p.IMApprovedAt)
	p.TrusteeApprovedAt = utcPtr(p.TrusteeApprovedAt)
	p.ReversedAt = utcPtr(p.ReversedAt)
	p.ClosedAt = utcPtr(p.ClosedAt)

	return p, nil
}

// utcPtr normalises an optional timestamp to UTC.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// ByID loads one period.
func (s Periods) ByID(ctx context.Context, id string) (Period, error) {
	const q = `SELECT ` + periodColumns + ` FROM distribution_periods WHERE id = $1`

	p, err := scanPeriod(s.q.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Period{}, ErrNotFound
	}
	if err != nil {
		return Period{}, err
	}
	return p, nil
}

// ForScheme lists a scheme's periods, most recent first.
//
// Ordered by period_seq, which is unique per scheme and monotonic, so paging is stable without a tiebreak.
func (s Periods) ForScheme(ctx context.Context, schemeID string, p Pagination) ([]Period, error) {
	p = p.normalise()

	const q = `
		SELECT ` + periodColumns + `
		FROM distribution_periods
		WHERE scheme_id = $1
		ORDER BY period_seq DESC
		LIMIT $2`

	rows, err := s.q.Query(ctx, q, schemeID, p.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Period
	for rows.Next() {
		period, err := scanPeriod(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, period)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// SnapshotFor loads the register snapshot for a period.
//
// Returns ErrNotFound when the register has not been frozen yet, which is a normal state for an open period
// rather than an error. The caller decides whether its absence matters.
func (s Periods) SnapshotFor(ctx context.Context, periodID string) (Snapshot, error) {
	const q = `
		SELECT
			rs.id, rs.distribution_period_id, rs.record_date, rs.taken_at,
			rs.total_units, rs.distinct_holders,
			rs.snapshot_merkle_root, rs.snapshot_cid_digest,
			coalesce(rs.ipfs_cid, ''), rs.anchor_status::text, coalesce(rs.anchored_tx, ''),
			(SELECT count(*) FROM register_snapshot_lines l WHERE l.snapshot_id = rs.id)
		FROM register_snapshots rs
		WHERE rs.distribution_period_id = $1`

	var snap Snapshot
	err := s.q.QueryRow(ctx, q, periodID).Scan(
		&snap.ID, &snap.PeriodID, &snap.RecordDate, &snap.TakenAt,
		&snap.TotalUnits, &snap.DistinctHolders,
		&snap.MerkleRoot, &snap.CIDDigest,
		&snap.IPFSCid, &snap.AnchorStatus, &snap.AnchoredTx,
		&snap.LineCount,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, err
	}

	snap.RecordDate = snap.RecordDate.UTC()
	snap.TakenAt = snap.TakenAt.UTC()
	return snap, nil
}
