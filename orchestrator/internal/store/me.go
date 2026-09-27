package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/acresync/orchestrator/internal/entitlement"
	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/payout"
)

// An investor's own records.
//
// # Scoping
//
// Every query in this file takes an investor id and filters on it in SQL. None of them loads a set and filters
// in Go, and none of them accepts an id from a path parameter: the caller's identity comes from their verified
// token. Filtering after the fact would mean the wrong row had already been read, and one forgotten check would
// hand somebody another unitholder's position.
//
// # Encrypted columns are not read
//
// full_name_enc, pan_enc, email_enc, phone_enc and account_number_enc hold application-layer ciphertext under a
// KMS-wrapped data key. There is no decryption path in this system yet, and this package does not invent one.
// So a display name, a masked PAN and a masked account number are absent rather than guessed at, and the
// contract types each of them as optional or nullable, which is what makes that honest rather than a gap.
//
// demat_accounts.client_id is plaintext, so that one can genuinely be masked.

// Identity is the investor's own profile.
type Identity struct {
	InvestorID    string
	InvestorClass entitlement.InvestorClass
	KYCStatus     string
	IsIMRelated   bool

	// WalletAddress is the active wallet, empty if none.
	WalletAddress string

	Demats []DematAccount
	Banks  []BankAccount
}

// DematAccount is a depository account, with only what can be shown.
type DematAccount struct {
	ID         string
	Depository string
	// MaskedClientID shows the last four characters. client_id is plaintext in the schema, so this is a real
	// mask rather than a placeholder.
	MaskedClientID string
	Verified       bool
}

// BankAccount is a settlement account.
//
// No masked account number: the column is ciphertext and nothing here can decrypt it. IFSC is plaintext and is
// not itself identifying of a person, so it is shown.
type BankAccount struct {
	ID       string
	IFSC     string
	Verified bool
}

// Holding is a position in a scheme.
type Holding struct {
	SchemeID                string
	SchemeName              string
	WalletAddress           string
	Units                   int32
	ExcludedFromHolderCount bool
	LockInExpiresAt         *time.Time
	FirstCreditedAt         time.Time
}

// Entitlement is a distribution entitlement with its tax treatment and payout.
type Entitlement struct {
	PeriodID           string
	PeriodEnd          time.Time
	LeafIndex          int32
	Units              int32
	SnapshotTotalUnits int32
	GrossPaise         money.Paise

	// Tax is absent until the deduction is computed.
	Tax *TaxDeduction

	// NetPayablePaise is the tax deduction's net figure, or the gross when no deduction exists yet.
	NetPayablePaise money.Paise

	// Payout is absent until one is instructed.
	Payout *Payout
}

// TaxDeduction is the withholding applied to an entitlement.
type TaxDeduction struct {
	InvestorClass  entitlement.InvestorClass
	Section        string
	RateBps        int32
	AmountPaise    money.Paise
	Form15GHOnFile bool
}

// Payout is a payout instruction.
type Payout struct {
	ID          string
	PeriodID    string
	AmountPaise money.Paise

	// Status is our own vocabulary, not the provider's. payout has two: DBStatus is ours and uppercase, and
	// the provider's is lowercase and theirs to change. Only ours is ever exposed.
	Status payout.DBStatus

	UTR         string
	SettledAt   *time.Time
	FailureCode string
	Provider    string
}

// Simulated reports whether the payout went through a mock provider.
//
// Surfaced deliberately so a demo can never be mistaken for a payment. The final fiat hop is simulated while
// corporate banking KYC is outstanding, and hiding that would be the one dishonest thing in the system.
func (p Payout) Simulated() bool { return p.Provider == "MOCK" }

// Me reads an investor's own records.
type Me struct {
	q Querier
}

// NewMe binds a store to a pool or transaction.
func NewMe(q Querier) Me { return Me{q: q} }

// Identity loads the investor's profile.
//
// The KYC status is the most recent record rather than an aggregate: a later verification supersedes an earlier
// one, and reporting the earliest or an arbitrary row would state the wrong thing about whether the holder can
// transact.
func (s Me) Identity(ctx context.Context, investorID string) (Identity, error) {
	const q = `
		SELECT
			i.id,
			i.investor_class::text,
			coalesce((
				SELECT k.status::text FROM kyc_records k
				WHERE k.investor_id = i.id
				ORDER BY k.created_at DESC
				LIMIT 1
			), 'PENDING'),
			i.is_im_related,
			coalesce((
				SELECT w.address FROM wallets w
				WHERE w.investor_id = i.id AND w.is_active
				ORDER BY w.activated_at DESC
				LIMIT 1
			), '')
		FROM investors i
		WHERE i.id = $1`

	var (
		out       Identity
		classText string
	)
	err := s.q.QueryRow(ctx, q, investorID).Scan(
		&out.InvestorID, &classText, &out.KYCStatus, &out.IsIMRelated, &out.WalletAddress)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrNotFound
	}
	if err != nil {
		return Identity{}, err
	}

	out.InvestorClass = entitlement.InvestorClass(classText)
	if !out.InvestorClass.Valid() {
		return Identity{}, unknownEnum("investors.investor_class", classText)
	}

	if out.Demats, err = s.demats(ctx, investorID); err != nil {
		return Identity{}, err
	}
	if out.Banks, err = s.banks(ctx, investorID); err != nil {
		return Identity{}, err
	}
	return out, nil
}

// mask shows the last four characters of a plaintext identifier.
//
// A value too short to mask is replaced entirely rather than partly revealed. Showing three of four characters
// of a short identifier is not a mask.
const maskVisible = 4

func mask(s string) string {
	if len(s) <= maskVisible {
		return "****"
	}
	return "****" + s[len(s)-maskVisible:]
}

func (s Me) demats(ctx context.Context, investorID string) ([]DematAccount, error) {
	const q = `
		SELECT id, depository::text, client_id, verified_at IS NOT NULL
		FROM demat_accounts
		WHERE investor_id = $1
		ORDER BY is_primary DESC, created_at`

	rows, err := s.q.Query(ctx, q, investorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DematAccount
	for rows.Next() {
		var (
			d        DematAccount
			clientID string
		)
		if err := rows.Scan(&d.ID, &d.Depository, &clientID, &d.Verified); err != nil {
			return nil, err
		}
		d.MaskedClientID = mask(clientID)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s Me) banks(ctx context.Context, investorID string) ([]BankAccount, error) {
	// account_number_enc is deliberately not selected. It cannot be decrypted here, and selecting ciphertext
	// only to discard it would put it in a query plan and a result set for no purpose.
	const q = `
		SELECT id, ifsc, verified_at IS NOT NULL
		FROM bank_accounts
		WHERE investor_id = $1
		ORDER BY created_at`

	rows, err := s.q.Query(ctx, q, investorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []BankAccount
	for rows.Next() {
		var b BankAccount
		if err := rows.Scan(&b.ID, &b.IFSC, &b.Verified); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Holdings lists the investor's positions.
//
// Zero-unit rows are included. A holding that has fallen to zero is a fact about the register worth showing,
// and hiding it would make a position that was sold look as though it never existed.
func (s Me) Holdings(ctx context.Context, investorID string, p Pagination) ([]Holding, error) {
	p = p.normalise()

	const q = `
		SELECT h.scheme_id, s.name, h.wallet_address, h.units,
		       h.is_excluded_from_holder_count, h.lock_in_expires_at, h.first_credited_at
		FROM unit_holdings h
		JOIN schemes s ON s.id = h.scheme_id
		WHERE h.investor_id = $1
		ORDER BY h.first_credited_at DESC, h.scheme_id
		LIMIT $2`

	rows, err := s.q.Query(ctx, q, investorID, p.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Holding
	for rows.Next() {
		var h Holding
		if err := rows.Scan(&h.SchemeID, &h.SchemeName, &h.WalletAddress, &h.Units,
			&h.ExcludedFromHolderCount, &h.LockInExpiresAt, &h.FirstCreditedAt); err != nil {
			return nil, err
		}
		h.FirstCreditedAt = h.FirstCreditedAt.UTC()
		h.LockInExpiresAt = utcPtr(h.LockInExpiresAt)
		out = append(out, h)
	}
	return out, rows.Err()
}

// Entitlements lists the investor's distribution entitlements.
//
// Joined to the snapshot line for the leaf index, which is what makes an entitlement checkable: with the index
// and the published snapshot root, a holder can verify their own line was in the tree the chain recorded.
//
// The tax deduction and the payout are both left joined, because an entitlement exists before either. Reporting
// a missing deduction as zero tax would state that none is due, which is a different claim from not yet
// computed.
func (s Me) Entitlements(ctx context.Context, investorID string, p Pagination) ([]Entitlement, error) {
	p = p.normalise()

	const q = `
		SELECT
			e.distribution_period_id, dp.period_end, l.leaf_index,
			e.units, e.snapshot_total_units, e.gross_entitlement_paise,
			t.investor_class::text, t.tds_section, t.tds_rate_bps, t.tds_amount_paise,
			t.net_payable_paise, t.form_15g_h_on_file,
			pi.id, pi.amount_paise, pi.status::text, pi.utr, pi.settled_at,
			pi.failure_code, pi.provider::text
		FROM entitlements e
		JOIN distribution_periods dp ON dp.id = e.distribution_period_id
		JOIN register_snapshot_lines l ON l.id = e.snapshot_line_id
		LEFT JOIN tax_deductions t ON t.entitlement_id = e.id
		LEFT JOIN payout_instructions pi ON pi.entitlement_id = e.id
		WHERE e.investor_id = $1
		ORDER BY dp.period_end DESC, e.distribution_period_id
		LIMIT $2`

	rows, err := s.q.Query(ctx, q, investorID, p.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Entitlement
	for rows.Next() {
		var (
			e Entitlement

			taxClass   *string
			taxSection *string
			taxRate    *int32
			taxAmount  *int64
			netPayable *int64
			form15GH   *bool

			payoutID     *string
			payoutAmount *int64
			payoutStatus *string
			payoutUTR    *string
			payoutSettle *time.Time
			payoutFail   *string
			payoutProv   *string

			gross int64
		)

		if err := rows.Scan(
			&e.PeriodID, &e.PeriodEnd, &e.LeafIndex,
			&e.Units, &e.SnapshotTotalUnits, &gross,
			&taxClass, &taxSection, &taxRate, &taxAmount, &netPayable, &form15GH,
			&payoutID, &payoutAmount, &payoutStatus, &payoutUTR, &payoutSettle,
			&payoutFail, &payoutProv,
		); err != nil {
			return nil, err
		}

		e.GrossPaise = money.Paise(gross)
		e.PeriodEnd = e.PeriodEnd.UTC()

		if taxClass != nil && taxSection != nil && taxRate != nil && taxAmount != nil {
			class := entitlement.InvestorClass(*taxClass)
			if !class.Valid() {
				return nil, unknownEnum("tax_deductions.investor_class", *taxClass)
			}
			e.Tax = &TaxDeduction{
				InvestorClass: class,
				Section:       *taxSection,
				RateBps:       *taxRate,
				AmountPaise:   money.Paise(*taxAmount),
			}
			if form15GH != nil {
				e.Tax.Form15GHOnFile = *form15GH
			}
		}

		// Net is the deduction's figure when one exists. Before that it is the gross, because nothing has been
		// withheld yet; defaulting it to zero would report the holder as owed nothing.
		if netPayable != nil {
			e.NetPayablePaise = money.Paise(*netPayable)
		} else {
			e.NetPayablePaise = e.GrossPaise
		}

		if payoutID != nil && payoutStatus != nil && payoutAmount != nil {
			status := payout.DBStatus(*payoutStatus)
			if !status.Valid() {
				return nil, unknownEnum("payout_instructions.status", *payoutStatus)
			}
			p := &Payout{
				ID:          *payoutID,
				PeriodID:    e.PeriodID,
				AmountPaise: money.Paise(*payoutAmount),
				Status:      status,
				SettledAt:   utcPtr(payoutSettle),
			}
			if payoutUTR != nil {
				p.UTR = *payoutUTR
			}
			if payoutFail != nil {
				p.FailureCode = *payoutFail
			}
			if payoutProv != nil {
				p.Provider = *payoutProv
			}
			e.Payout = p
		}

		out = append(out, e)
	}
	return out, rows.Err()
}

// Payouts lists the investor's payout instructions.
func (s Me) Payouts(ctx context.Context, investorID string, p Pagination) ([]Payout, error) {
	p = p.normalise()

	const q = `
		SELECT pi.id, e.distribution_period_id, pi.amount_paise, pi.status::text,
		       coalesce(pi.utr, ''), pi.settled_at, coalesce(pi.failure_code, ''), pi.provider::text
		FROM payout_instructions pi
		JOIN entitlements e ON e.id = pi.entitlement_id
		WHERE e.investor_id = $1
		ORDER BY pi.created_at DESC
		LIMIT $2`

	rows, err := s.q.Query(ctx, q, investorID, p.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Payout
	for rows.Next() {
		var (
			p          Payout
			amount     int64
			statusText string
		)
		if err := rows.Scan(&p.ID, &p.PeriodID, &amount, &statusText,
			&p.UTR, &p.SettledAt, &p.FailureCode, &p.Provider); err != nil {
			return nil, err
		}

		p.AmountPaise = money.Paise(amount)
		p.Status = payout.DBStatus(statusText)
		if !p.Status.Valid() {
			return nil, unknownEnum("payout_instructions.status", statusText)
		}
		p.SettledAt = utcPtr(p.SettledAt)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Bid is an investor's bid on an offer.
type Bid struct {
	ID              string
	OfferID         string
	BidReference    string
	Units           int32
	PricePerUnit    money.Paise
	TotalAmount     money.Paise
	Status          string
	RejectionReason string
	SubmittedAt     time.Time

	// BlockStatus is the ASBA funds block, empty when none has been requested.
	BlockStatus  string
	BlockedPaise *money.Paise
}

// Bids lists the investor's bids.
func (s Me) Bids(ctx context.Context, investorID string, p Pagination) ([]Bid, error) {
	p = p.normalise()

	const q = `
		SELECT b.id, b.offer_id, b.bid_reference, b.units_bid,
		       b.price_per_unit_paise, b.total_amount_paise,
		       b.status::text, coalesce(b.rejection_reason::text, ''), b.submitted_at,
		       coalesce(a.block_status::text, ''), a.blocked_amount_paise
		FROM bids b
		LEFT JOIN asba_blocks a ON a.bid_id = b.id
		WHERE b.investor_id = $1
		ORDER BY b.submitted_at DESC
		LIMIT $2`

	rows, err := s.q.Query(ctx, q, investorID, p.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Bid
	for rows.Next() {
		var (
			b       Bid
			price   int64
			total   int64
			blocked *int64
		)
		if err := rows.Scan(&b.ID, &b.OfferID, &b.BidReference, &b.Units,
			&price, &total, &b.Status, &b.RejectionReason, &b.SubmittedAt,
			&b.BlockStatus, &blocked); err != nil {
			return nil, err
		}

		b.PricePerUnit = money.Paise(price)
		b.TotalAmount = money.Paise(total)
		b.SubmittedAt = b.SubmittedAt.UTC()
		if blocked != nil {
			v := money.Paise(*blocked)
			b.BlockedPaise = &v
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
