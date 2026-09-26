package e2e

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/asba"
	"github.com/acresync/orchestrator/internal/ballot"
	"github.com/acresync/orchestrator/internal/ballotrun"
	"github.com/acresync/orchestrator/internal/bidbook"
	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/ipfs"
	"github.com/acresync/orchestrator/internal/ipfsguard"
	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/money"
	"github.com/acresync/orchestrator/internal/offer"
	"github.com/acresync/orchestrator/internal/outbox"
	"github.com/acresync/orchestrator/internal/settlement"
)

// TestPrimaryMarketOfferToSettledCapTable drives the whole primary market against a real Postgres.
//
// # What this catches that the unit tests cannot
//
// Each package is already tested against its own contract. What none of them can see is the join: a
// figure computed in one package and refused by a constraint declared in another, a document that
// validates on a hand-built fixture and fails once real rows fill it, or an ordering the orchestrator
// believes in that a trigger disagrees with.
//
// The ballot's reveal ordering is the clearest example. internal/ballotrun enforces it in Go and
// migration 0008 enforces it in plpgsql, and until both run against the same rows in the same sequence
// there is no evidence they agree about what the sequence is.
func TestPrimaryMarketOfferToSettledCapTable(t *testing.T) {
	ctx := context.Background()
	f := newPrimaryFixture(t)

	r := &primaryRun{f: f, t: t}

	r.createOffer(ctx)
	r.openOffer(ctx)
	r.submitBids(ctx)
	r.blockFunds(ctx)
	r.closeOffer(ctx)
	r.freezeBook(ctx)
	r.anchorBook(ctx)
	r.commitSeed(ctx)
	r.revealSeed(ctx)
	r.drawBallot(ctx)
	r.recordAllocations(ctx)
	r.anchorResult(ctx)
	r.settleAsba(ctx)
	r.creditRegister(ctx)
	r.assertCapTable(ctx)
	r.assertMoneyReconciles(ctx)
	r.assertPublishedDocumentsVerify(ctx)
}

// primaryRun carries state between the stages of the one long test.
type primaryRun struct {
	f *primaryFixture
	t *testing.T

	bidIDs  []string
	bidRefs []string
	blocks  []*asba.Block

	provider *asba.Mock

	book        *bidbook.Book
	bookPin     *ipfs.Pin
	ballotRunID string

	ceremony *ballotrun.Ceremony
	secret   merkle.Hash

	draw       *ballotrun.Run
	allotPin   *ipfs.Pin
	plan       *settlement.Plan
	publisher  *ipfs.Publisher
	targetHash merkle.Hash

	// nonce is the relayer's transaction sequence. One per scheme, because nonces are one sequence per
	// sending address.
	nonce uint64

	// ev accumulates as the offer progresses, which is how the guards are designed to be used: each
	// transition is judged against everything established so far, not against a snapshot assembled for
	// the occasion. Building it up here means a stage that forgets to record its artefact fails the
	// next guard, exactly as it would in the orchestrator.
	ev offer.Evidence
}

// confirmedAnchor is an outbox row the chain has confirmed to the required depth.
func confirmedAnchor() *offer.AnchorRef {
	return &offer.AnchorRef{
		Status:        "CONFIRMED",
		Confirmations: offer.MinAnchorConfirmations,
		TxHash:        "0x" + fmt.Sprintf("%064x", 1),
	}
}

func (r *primaryRun) key(action idempotency.Action, scope string, payload map[string]any) []byte {
	r.t.Helper()
	k, err := idempotency.Derive(idempotency.Input{
		Action: action, SchemeID: r.f.schemeID, ScopeID: scope, Payload: payload,
	})
	if err != nil {
		r.t.Fatalf("deriving a %s key: %v", action, err)
	}
	return k[:]
}

// --- stage 1: the offer ---------------------------------------------------------------------------

func (r *primaryRun) createOffer(ctx context.Context) {
	t := r.t

	// The Go terms and the table's CHECK constraints describe the same offer. Validating first means a
	// violation is reported in business language rather than as a constraint name.
	terms := offer.Terms{
		UnitsOnOffer:         primaryOnOffer,
		MinBidUnits:          primaryMinBid,
		MaxBidUnits:          primaryMaxBid,
		MinSubscriptionUnits: primaryMinSub,
		MinDistinctHolders:   primaryMinHold,
		PriceBandLowerPaise:  money.MinUnitPricePaise,
		PriceBandUpperPaise:  primaryBandHigh,
		OpensAt:              offerOpens,
		ClosesAt:             offerCloses,
		AllotmentDueAt:       allotmentDue,
	}
	if err := terms.Validate(); err != nil {
		t.Fatalf("the fixture offer terms must be valid: %v", err)
	}

	// Business time, supplied rather than read, so nothing in this test depends on when it runs.
	r.ev.Terms = terms
	r.ev.Now = offerOpens

	if err := r.f.pool.QueryRow(ctx, `
		INSERT INTO offers (
			scheme_id, offer_type, price_band_lower_paise, price_band_upper_paise,
			units_on_offer, min_bid_units, max_bid_units, min_subscription_units,
			min_distinct_holders, opens_at, closes_at, allotment_due_at, status, idempotency_key
		) VALUES ($1, 'INITIAL', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'CONFIGURED', $12)
		RETURNING id`,
		r.f.schemeID,
		int64(terms.PriceBandLowerPaise), int64(terms.PriceBandUpperPaise),
		terms.UnitsOnOffer, terms.MinBidUnits, terms.MaxBidUnits,
		terms.MinSubscriptionUnits, terms.MinDistinctHolders,
		offerOpens, offerCloses, allotmentDue,
		r.key(idempotency.ActionCreateOffer, "", map[string]any{"sebiRef": r.f.sebiRef}),
	).Scan(&r.f.offerID); err != nil {
		t.Fatalf("creating the offer: %v", err)
	}
}

func (r *primaryRun) openOffer(ctx context.Context) {
	r.advanceOffer(ctx, offer.StatusConfigured, offer.StatusOpen)
}

// advanceOffer moves the offer through the Go guard first, then the database.
//
// The guard is what decides. The UPDATE is a record of a decision already made, which is the point:
// if the guard and the schema disagreed about a legal transition, this is where it surfaces.
func (r *primaryRun) advanceOffer(ctx context.Context, from, to offer.Status) {
	t := r.t
	t.Helper()

	if err := offer.Guard(from, to, r.ev); err != nil {
		t.Fatalf("the guard refuses %s -> %s: %v", from, to, err)
	}

	var got string
	if err := r.f.pool.QueryRow(ctx, `
		UPDATE offers SET status = $2 WHERE id = $1 AND status = $3 RETURNING status`,
		r.f.offerID, string(to), string(from),
	).Scan(&got); err != nil {
		t.Fatalf("advancing the offer %s -> %s: %v", from, to, err)
	}
	if got != string(to) {
		t.Fatalf("the offer is %s, want %s", got, to)
	}
}

// --- stage 2: bids and blocked funds ---------------------------------------------------------------

func (r *primaryRun) submitBids(ctx context.Context) {
	t := r.t

	for i := range r.f.investorIDs {
		ref := bidReference(i)
		total := int64(unitsPerBid) * int64(bidPricePaise)

		var id string
		if err := r.f.pool.QueryRow(ctx, `
			INSERT INTO bids (
				offer_id, investor_id, investor_anchor_hash, demat_account_id, bank_account_id,
				units_bid, price_per_unit_paise, total_amount_paise, bid_reference,
				status, is_synthetic, idempotency_key
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'SUBMITTED', TRUE, $10)
			RETURNING id`,
			r.f.offerID, r.f.investorIDs[i], r.f.anchors[i][:],
			r.f.dematIDs[i], r.f.bankIDs[i],
			unitsPerBid, int64(bidPricePaise), total, ref,
			r.key(idempotency.ActionSubmitBid, r.f.offerID, map[string]any{"bidRef": ref}),
		).Scan(&id); err != nil {
			// bids_within_offer_bounds fires here if the units or price fall outside the offer, so a
			// failure at this point is the trigger doing its job on a bad fixture.
			t.Fatalf("submitting bid %d: %v", i, err)
		}

		r.bidIDs = append(r.bidIDs, id)
		r.bidRefs = append(r.bidRefs, ref)
	}

	if len(r.bidIDs) != primaryBidders {
		t.Fatalf("submitted %d bids, want %d", len(r.bidIDs), primaryBidders)
	}

	r.ev.Book.BidCount = primaryBidders
	// Nothing is validated yet, so the book is not freezable at this point. The guard is exercised on
	// that below, once the blocks land.
	r.ev.Book.PendingValidation = primaryBidders
}

// blockFunds runs every bid through the ASBA provider and records the result.
//
// Money never leaves the investor's account here. The provider marks it unavailable and the row records
// that, which is why there is no house balance anywhere in this flow to assert against.
func (r *primaryRun) blockFunds(ctx context.Context) {
	t := r.t

	r.provider = asba.NewMock(fixedBusinessClock{at: bookFrozenAt})

	amount := money.Paise(int64(unitsPerBid) * int64(bidPricePaise))

	for i, bidID := range r.bidIDs {
		accountRef := fmt.Sprintf("acct-%03d", i)
		mandateRef := fmt.Sprintf("mandate-%03d", i)

		// Funded with exactly the bid amount. A block is all or nothing, so an account short by one
		// paise must fail rather than partially reserve, and exact funding is the strictest version of
		// that test.
		r.provider.Fund(accountRef, amount, mandateRef)

		req := asba.BlockRequest{
			IdempotencyKey: fmt.Sprintf("%x", r.key(idempotency.ActionRequestASBABlock,
				r.f.offerID, map[string]any{"bidRef": r.bidRefs[i]})),
			BidID:          bidID,
			BankAccountRef: accountRef,
			AmountPaise:    amount,
			MandateRef:     mandateRef,
		}

		block, err := r.provider.RequestBlock(ctx, req)
		if err != nil {
			t.Fatalf("blocking funds for bid %d: %v", i, err)
		}
		if !block.Status.HoldsFunds() {
			t.Fatalf("bid %d is %s, want BLOCKED", i, block.Status)
		}
		if err := block.Reconcile(); err != nil {
			t.Fatalf("bid %d block does not reconcile: %v", i, err)
		}

		// The investor's money is unavailable but still theirs.
		if got := r.provider.Available(accountRef); got != 0 {
			t.Fatalf("bid %d leaves %d available, want 0", i, got)
		}
		if got := r.provider.Held(accountRef); got != amount {
			t.Fatalf("bid %d holds %d, want %d", i, got, amount)
		}

		if _, err := r.f.pool.Exec(ctx, `
			INSERT INTO asba_blocks (
				bid_id, provider, bank_ref, requested_amount_paise, blocked_amount_paise,
				block_status, requested_at, blocked_at, idempotency_key
			) VALUES ($1, 'RAZORPAYX_SANDBOX', $2, $3, $4, 'BLOCKED', $5, $6, $7)`,
			bidID, block.BankRef, int64(amount), int64(block.BlockedPaise),
			block.RequestedAt, block.BlockedAt,
			r.key(idempotency.ActionRequestASBABlock, r.f.offerID,
				map[string]any{"block": r.bidRefs[i]}),
		); err != nil {
			t.Fatalf("recording the block for bid %d: %v", i, err)
		}

		if _, err := r.f.pool.Exec(ctx,
			`UPDATE bids SET status = 'FUNDS_BLOCKED' WHERE id = $1`, bidID); err != nil {
			t.Fatalf("marking bid %d funded: %v", i, err)
		}

		r.blocks = append(r.blocks, block)
	}

	// Every bid reserved its own money. A double block would show up as more blocks than bids.
	if got := r.provider.Count(); got != primaryBidders {
		t.Fatalf("the provider holds %d blocks for %d bids", got, primaryBidders)
	}

	r.ev.Book.FundsBlockedCount = primaryBidders
}

func (r *primaryRun) closeOffer(ctx context.Context) {
	t := r.t

	r.ev.Now = offerCloses
	r.advanceOffer(ctx, offer.StatusOpen, offer.StatusClosed)

	// Closing stops intake; it does not fix the book. While any bid is still unresolved the guard must
	// refuse to freeze, because a book whose membership can still change must not have a root computed
	// over it.
	if err := offer.Guard(offer.StatusClosed, offer.StatusBookFrozen, r.ev); err == nil {
		t.Fatal("freezing must be refused while bids are still awaiting validation")
	}

	if _, err := r.f.pool.Exec(ctx,
		`UPDATE bids SET status = 'IN_BOOK' WHERE offer_id = $1 AND status = 'FUNDS_BLOCKED'`,
		r.f.offerID); err != nil {
		t.Fatalf("admitting bids to the book: %v", err)
	}

	var inBook int
	if err := r.f.pool.QueryRow(ctx,
		`SELECT count(*) FROM bids WHERE offer_id = $1 AND status = 'IN_BOOK'`,
		r.f.offerID).Scan(&inBook); err != nil {
		t.Fatal(err)
	}
	if inBook != primaryBidders {
		t.Fatalf("%d bids are in the book, want %d", inBook, primaryBidders)
	}

	r.ev.Book.InBookCount = inBook
	r.ev.Book.PendingValidation = 0
	r.ev.Book.UnitsBid = primaryBidders * unitsPerBid
	r.ev.Book.DistinctBidders = primaryBidders
}

// --- stage 3: freeze, pin, anchor -----------------------------------------------------------------

// freezeBook reads the book back out of Postgres rather than reusing what was written.
//
// Reusing the in-memory values would test the freeze against itself. Reading the rows means the root is
// computed from what was actually persisted, including anything a column type or a default quietly
// changed on the way in.
func (r *primaryRun) freezeBook(ctx context.Context) {
	t := r.t

	rows, err := r.f.pool.Query(ctx, `
		SELECT b.id, b.investor_id, b.bid_reference, b.investor_anchor_hash,
		       b.units_bid, b.price_per_unit_paise,
		       a.requested_amount_paise, a.blocked_amount_paise, a.block_status,
		       a.requested_at, a.blocked_at, a.bank_ref
		  FROM bids b
		  JOIN asba_blocks a ON a.bid_id = b.id
		 WHERE b.offer_id = $1 AND b.status = 'IN_BOOK'`,
		r.f.offerID)
	if err != nil {
		t.Fatalf("reading the book: %v", err)
	}
	defer rows.Close()

	var entries []bidbook.Entry
	for rows.Next() {
		var (
			bidID, investorID, ref, status string
			anchorBytes                    []byte
			units                          int32
			price, requested, blocked      int64
			requestedAt                    time.Time
			blockedAt                      *time.Time
			bankRef                        *string
		)
		if err := rows.Scan(&bidID, &investorID, &ref, &anchorBytes, &units, &price,
			&requested, &blocked, &status, &requestedAt, &blockedAt, &bankRef); err != nil {
			t.Fatal(err)
		}

		// The bank's own handle for the block. Carried through the freeze because the settle instruction
		// is addressed to it: without it the allotment could be computed and never paid for.
		if bankRef == nil || *bankRef == "" {
			t.Fatalf("bid %s has a block with no bank reference, so it could never be settled", bidID)
		}

		var anchor merkle.Hash
		copy(anchor[:], anchorBytes)

		entries = append(entries, bidbook.Entry{
			BidID:             bidID,
			InvestorID:        investorID,
			BidRef:            ref,
			InvestorAnchor:    anchor,
			UnitsBid:          uint32(units),
			PricePerUnitPaise: money.Paise(price),
			Block: &asba.Block{
				BidID:          bidID,
				BankRef:        *bankRef,
				Status:         asba.BlockStatus(status),
				RequestedPaise: money.Paise(requested),
				BlockedPaise:   money.Paise(blocked),
				RequestedAt:    requestedAt,
				BlockedAt:      blockedAt,
			},
		})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(entries) != primaryBidders {
		t.Fatalf("the book has %d entries, want %d", len(entries), primaryBidders)
	}

	book, err := bidbook.Freeze(bidbook.FreezeInput{
		SchemeRef: r.f.schemeRef,
		SchemeID:  r.f.schemeID,
		OfferID:   r.f.offerID,
		FrozenAt:  bookFrozenAt,
		Entries:   entries,
	})
	if err != nil {
		t.Fatalf("freezing the book: %v", err)
	}
	r.book = book

	if book.TotalUnitsBid != primaryBidders*unitsPerBid {
		t.Fatalf("the book bids %d units, want %d", book.TotalUnitsBid, primaryBidders*unitsPerBid)
	}

	// Feasibility is tested before anything is anchored, because anchoring is the first irreversible
	// public act.
	feas, err := ballot.CheckFeasibility(book.BallotBids(), ballot.V1Params())
	if err != nil {
		t.Fatalf("checking feasibility: %v", err)
	}
	if !feas.Feasible {
		t.Fatalf("the fixture offer must be feasible: %v", feas.Reasons)
	}

	r.advanceOffer(ctx, offer.StatusClosed, offer.StatusBookFrozen)

	r.ev.Feasible = true
	r.advanceOffer(ctx, offer.StatusBookFrozen, offer.StatusFeasibilityChecked)

	// The root exists now, but nothing is pinned. Anchoring must be refused: an anchored root committing
	// to a document nobody can fetch is a commitment to evidence that does not exist.
	r.ev.Ceremony.BidbookRoot = book.MerkleRoot
	if err := offer.Guard(offer.StatusFeasibilityChecked, offer.StatusBidbookAnchored, r.ev); err == nil {
		t.Fatal("anchoring must be refused before the bid book document is pinned")
	}
}

func (r *primaryRun) anchorBook(ctx context.Context) {
	t := r.t

	mock, err := ipfs.NewMockProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.publisher = ipfs.NewPublisher(mock)

	raw, err := r.book.Document()
	if err != nil {
		t.Fatalf("rendering the bid book: %v", err)
	}
	pin, err := r.publisher.Publish(ctx, ipfsguard.DocBidbook, raw)
	if err != nil {
		t.Fatalf("pinning the bid book: %v", err)
	}
	r.bookPin = pin

	req, err := r.book.AnchorRequest(pin)
	if err != nil {
		t.Fatalf("building the bid book anchor: %v", err)
	}

	num, den := r.book.OversubscriptionRatio(primaryOnOffer)

	// The ballot run row is created with the book already fixed. seed_commitment stays null: the trigger
	// refuses a commitment before a root exists, and here the root exists first by construction.
	if err := r.f.pool.QueryRow(ctx, `
		INSERT INTO ballot_runs (
			offer_id, bidbook_snapshot_at, bidbook_merkle_root, bidbook_cid_digest,
			bid_leaf_count, total_units_bid, distinct_bidders,
			oversubscription_num, oversubscription_den,
			algo_version, status, idempotency_key
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'BIDBOOK_ANCHORED', $11)
		RETURNING id`,
		r.f.offerID, bookFrozenAt, req.Root[:], req.CIDDigest[:],
		req.LeafCount, int64(req.TotalUnitsBid), req.DistinctBidders,
		int64(num), int64(den), ballot.AlgoVersion,
		r.key(idempotency.ActionAnchorBidbook, r.f.offerID,
			map[string]any{"root": req.Root.Hex()}),
	).Scan(&r.ballotRunID); err != nil {
		t.Fatalf("recording the ballot run: %v", err)
	}

	bookPayload, err := req.OutboxPayload()
	if err != nil {
		t.Fatal(err)
	}
	bookKey, err := r.book.IdempotencyKey(req)
	if err != nil {
		t.Fatal(err)
	}
	r.enqueueAnchor(ctx, "anchorBidbook", idempotency.ActionAnchorBidbook, bookPayload, bookKey)

	r.ev.BidbookPinned = true
	r.advanceOffer(ctx, offer.StatusFeasibilityChecked, offer.StatusBidbookAnchored)

	r.ev.Ceremony.BidbookAnchor = confirmedAnchor()
}

// enqueueAnchor puts a call through the real outbox and drives it to CONFIRMED.
//
// The transaction itself is exercised against Anvil in the relayer's own package. What matters here is
// that the anchor passes through the outbox's actual state graph rather than being written straight to
// CONFIRMED, because several offer guards and one database trigger depend on an anchor being genuinely
// confirmed, and a row inserted in its final state would prove nothing about how it got there.
func (r *primaryRun) enqueueAnchor(ctx context.Context, fn string, action idempotency.Action, payload []byte, key idempotency.Key) {
	t := r.t
	t.Helper()

	store := outbox.NewPostgresStore(r.f.pool)

	entry, err := store.Enqueue(ctx, outbox.NewEntry{
		SchemeID:       r.f.schemeID,
		TargetContract: schemeContract,
		FunctionName:   fn,
		Payload:        payload,
		IdempotencyKey: key,
		EnvironmentTag: "LOCAL",
	})
	if err != nil {
		t.Fatalf("enqueueing %s: %v", fn, err)
	}

	// The graph has to be walked. CONFIRMING before CONFIRMED is not decoration: it is the state in
	// which a reorg is still possible, and the store refuses to skip it.
	if _, err := store.ClaimNextQueued(ctx, r.f.schemeID, r.nonce); err != nil {
		t.Fatalf("claiming %s: %v", fn, err)
	}
	r.nonce++

	txHash := fmt.Sprintf("0x%064x", sha256.Sum256([]byte(fn+string(payload))))
	if err := store.MarkBroadcast(ctx, entry.ID, txHash, "1000000000"); err != nil {
		t.Fatalf("broadcasting %s: %v", fn, err)
	}
	if err := store.MarkConfirming(ctx, entry.ID, 9_000_200, 1); err != nil {
		t.Fatalf("confirming %s: %v", fn, err)
	}
	if err := store.MarkConfirmed(ctx, entry.ID, 9_000_200, offer.MinAnchorConfirmations); err != nil {
		t.Fatalf("finalising %s: %v", fn, err)
	}

	got, err := store.Get(ctx, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != outbox.StatusConfirmed {
		t.Fatalf("%s is %s, want CONFIRMED", fn, got.Status)
	}
}

// --- stage 4: the ceremony -------------------------------------------------------------------------

func (r *primaryRun) commitSeed(ctx context.Context) {
	t := r.t

	secret, err := ballotrun.DeriveSecret(primaryPepper, r.f.schemeID, r.f.offerID)
	if err != nil {
		t.Fatalf("deriving the seed secret: %v", err)
	}
	r.secret = secret

	r.ceremony = &ballotrun.Ceremony{
		SchemeID:    r.f.schemeID,
		OfferID:     r.f.offerID,
		Stage:       ballotrun.StageBidbookAnchored,
		BidbookRoot: r.book.MerkleRoot,
		Commitment:  ballot.Commitment(secret),
		Config:      ballotrun.V1Config(),
	}

	if err := r.ceremony.CanCommit(); err != nil {
		t.Fatalf("the ceremony refuses to commit: %v", err)
	}

	// The contract chooses the target block. Modelled here as a fixed height, because what is being
	// tested is the ordering of the writes rather than the chain's arithmetic.
	const targetBlock = 9_000_100

	if _, err := r.f.pool.Exec(ctx, `
		UPDATE ballot_runs SET seed_commitment = $2, target_block = $3, attempt = 1,
		                       status = 'SEED_COMMITTED'
		 WHERE id = $1`,
		r.ballotRunID, r.ceremony.Commitment[:], int64(targetBlock),
	); err != nil {
		t.Fatalf("committing the seed: %v", err)
	}

	r.ceremony.Stage = ballotrun.StageSeedCommitted
	r.ceremony.TargetBlock = targetBlock
	r.ceremony.Attempt = 1

	r.ev.Ceremony.SeedCommitment = r.ceremony.Commitment
	r.ev.Ceremony.TargetBlock = targetBlock
	r.ev.Ceremony.Attempt = 1
	r.advanceOffer(ctx, offer.StatusBidbookAnchored, offer.StatusSeedCommitted)
}

// revealSeed writes the plaintext only after the commitment is recorded as anchored.
//
// This is the sequence migration 0008 enforces in plpgsql and internal/ballotrun enforces in Go. The
// reason both exist is that a plaintext on disk before the commitment is public leaves an operator free
// to claim a different secret was always intended, and the remedy has to survive whichever layer a future
// change goes around.
func (r *primaryRun) revealSeed(ctx context.Context) {
	t := r.t

	// Before the commitment is anchored, the Go layer and the database must both refuse.
	if err := r.ceremony.CanPersistPlaintext(); err == nil {
		t.Fatal("the plaintext must not be storable before the commitment is anchored")
	}

	_, err := r.f.pool.Exec(ctx,
		`UPDATE ballot_runs SET seed_plaintext = $2 WHERE id = $1`,
		r.ballotRunID, r.secret[:])
	if err == nil {
		t.Fatal("the reveal-order trigger must refuse a plaintext before the commitment is anchored")
	}

	// Anchoring the commitment is what unlocks the reveal.
	commitTx := fmt.Sprintf("0x%064x", sha256.Sum256([]byte("commitSeed tx")))
	if _, err := r.f.pool.Exec(ctx, `
		UPDATE ballot_runs SET commitment_anchored_tx = $2 WHERE id = $1`,
		r.ballotRunID, commitTx,
	); err != nil {
		t.Fatalf("anchoring the commitment: %v", err)
	}

	r.ceremony.CommitmentAnchored = true
	if err := r.ceremony.CanPersistPlaintext(); err != nil {
		t.Fatalf("once anchored the plaintext may be stored: %v", err)
	}

	// The observed hash of the target block, which the operator cannot choose.
	r.targetHash = sha256.Sum256([]byte("target block hash"))

	const revealAt = 9_000_150
	if err := r.ceremony.CanReveal(revealAt); err != nil {
		t.Fatalf("the reveal window should be open at block %d: %v", revealAt, err)
	}

	seed, err := r.ceremony.FinalSeed(r.secret, r.targetHash)
	if err != nil {
		t.Fatalf("deriving the final seed: %v", err)
	}

	if _, err := r.f.pool.Exec(ctx, `
		UPDATE ballot_runs
		   SET seed_plaintext = $2, target_block_hash = $3, final_seed = $4, status = 'SEED_REVEALED'
		 WHERE id = $1`,
		r.ballotRunID, r.secret[:], r.targetHash[:], seed[:],
	); err != nil {
		t.Fatalf("revealing the seed: %v", err)
	}

	// The commitment cannot be changed afterwards, whatever the orchestrator asks for.
	other := sha256.Sum256([]byte("a different secret"))
	if _, err := r.f.pool.Exec(ctx,
		`UPDATE ballot_runs SET seed_commitment = $2 WHERE id = $1`,
		r.ballotRunID, other[:]); err == nil {
		t.Fatal("seed_commitment must be immutable once set")
	}

	r.ceremony.Stage = ballotrun.StageSeedRevealed

	r.ev.Ceremony.CommitmentAnchor = confirmedAnchor()
	r.ev.Ceremony.TargetBlockHash = r.targetHash
	r.ev.Ceremony.SecretRevealed = true
	r.advanceOffer(ctx, offer.StatusSeedCommitted, offer.StatusSeedRevealed)
}

// --- stage 5: the draw ----------------------------------------------------------------------------

func (r *primaryRun) drawBallot(ctx context.Context) {
	t := r.t

	// The seed is read back from the database, not reused from memory. If a column silently altered a
	// byte the draw would diverge from the anchored root, and this is where that shows.
	var seedBytes, rootBytes []byte
	if err := r.f.pool.QueryRow(ctx,
		`SELECT final_seed, bidbook_merkle_root FROM ballot_runs WHERE id = $1`,
		r.ballotRunID).Scan(&seedBytes, &rootBytes); err != nil {
		t.Fatalf("reading back the seed: %v", err)
	}

	var storedRoot merkle.Hash
	copy(storedRoot[:], rootBytes)
	if storedRoot != r.book.MerkleRoot {
		t.Fatalf("the stored root %s is not the frozen root %s",
			storedRoot.Hex(), r.book.MerkleRoot.Hex())
	}

	draw, err := ballotrun.Draw(ballotrun.RunInput{
		Book:            r.book,
		Ceremony:        r.ceremony,
		Secret:          r.secret,
		TargetBlockHash: r.targetHash,
		Params:          ballot.V1Params(),
		DrawnAt:         ballotDrawnAt,
	})
	if err != nil {
		t.Fatalf("running the draw: %v", err)
	}
	r.draw = draw

	if err := draw.Validate(); err != nil {
		t.Fatalf("the draw must be anchorable: %v", err)
	}

	var storedSeed merkle.Hash
	copy(storedSeed[:], seedBytes)
	if storedSeed != draw.Result.FinalSeed {
		t.Fatalf("the stored seed %s produced a draw seeded with %s",
			storedSeed.Hex(), draw.Result.FinalSeed.Hex())
	}

	if draw.Result.UnitsAllotted != primaryOnOffer {
		t.Fatalf("allotted %d units, want all %d", draw.Result.UnitsAllotted, primaryOnOffer)
	}
	if draw.Result.DistinctAllottees < primaryMinHold {
		t.Fatalf("%d allottees, below the floor of %d",
			draw.Result.DistinctAllottees, primaryMinHold)
	}

	// The fixture must produce every outcome the downstream code branches on, or a path goes untested
	// while the test still passes. This is a guard on the fixture, not on the allocator.
	counts := map[ballot.Outcome]int{}
	for _, l := range draw.Lines {
		counts[l.Outcome]++
	}
	if counts[ballot.OutcomeNilBallot] == 0 {
		t.Fatal("the fixture must oversubscribe in bidders as well as units, or no bid is rejected " +
			"and the nil-outcome path never runs")
	}
	if counts[ballot.OutcomePartial]+counts[ballot.OutcomeFull] == 0 {
		t.Fatal("the fixture must produce at least one fill")
	}
	t.Logf("draw: %d full, %d partial, %d nil across %d bids",
		counts[ballot.OutcomeFull], counts[ballot.OutcomePartial],
		counts[ballot.OutcomeNilBallot], len(draw.Lines))
}

// recordAllocations writes one row per bid, winners and losers alike.
func (r *primaryRun) recordAllocations(ctx context.Context) {
	t := r.t

	for _, l := range r.draw.Lines {
		if _, err := r.f.pool.Exec(ctx, `
			INSERT INTO allocations (
				ballot_run_id, bid_id, units_allotted, amount_payable_paise,
				refund_amount_paise, outcome, ballot_rank
			) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			r.ballotRunID, l.BidID, l.UnitsAllotted,
			int64(l.AmountPayable), int64(l.RefundAmount),
			string(l.Outcome), l.BallotRank,
		); err != nil {
			// allocations_outcome_consistent fires here if a nil outcome carries units, or a fill
			// carries none.
			t.Fatalf("recording the allocation for leaf %d: %v", l.LeafIndex, err)
		}

		status := "REJECTED_BALLOT"
		switch l.Outcome {
		case ballot.OutcomeFull:
			status = "ALLOTTED_FULL"
		case ballot.OutcomePartial:
			status = "ALLOTTED_PARTIAL"
		}

		if status == "REJECTED_BALLOT" {
			// bids_rejection_reason_present pairs every REJECTED_* status with a reason.
			if _, err := r.f.pool.Exec(ctx,
				`UPDATE bids SET status = $2, rejection_reason = 'BALLOT_NOT_DRAWN' WHERE id = $1`,
				l.BidID, status); err != nil {
				t.Fatalf("rejecting bid %s: %v", l.BidID, err)
			}
			continue
		}
		if _, err := r.f.pool.Exec(ctx,
			`UPDATE bids SET status = $2 WHERE id = $1`, l.BidID, status); err != nil {
			t.Fatalf("marking bid %s allotted: %v", l.BidID, err)
		}
	}

	// Every bid has an outcome. A losing bidder needs a published rank to check, so an allocation count
	// short of the bid count would mean the draw was unfalsifiable for exactly those people.
	var allocated int
	if err := r.f.pool.QueryRow(ctx,
		`SELECT count(*) FROM allocations WHERE ballot_run_id = $1`, r.ballotRunID).
		Scan(&allocated); err != nil {
		t.Fatal(err)
	}
	if allocated != primaryBidders {
		t.Fatalf("%d allocations for %d bids", allocated, primaryBidders)
	}

	if _, err := r.f.pool.Exec(ctx,
		`UPDATE ballot_runs SET status = 'DRAWN', executed_at = $2 WHERE id = $1`,
		r.ballotRunID, ballotDrawnAt); err != nil {
		t.Fatal(err)
	}

	r.ev.Ceremony.FinalSeed = r.draw.Result.FinalSeed
	r.ev.Ceremony.ResultRoot = r.draw.Result.ResultRoot
	r.ev.AllocationsRecorded = allocated
	r.advanceOffer(ctx, offer.StatusSeedRevealed, offer.StatusBallotDrawn)
}

func (r *primaryRun) anchorResult(ctx context.Context) {
	t := r.t

	raw, err := r.draw.Document()
	if err != nil {
		t.Fatalf("rendering the allotment file: %v", err)
	}
	pin, err := r.publisher.Publish(ctx, ipfsguard.DocAllotmentFile, raw)
	if err != nil {
		t.Fatalf("pinning the allotment file: %v", err)
	}
	r.allotPin = pin

	req, err := r.draw.AnchorRequest(pin)
	if err != nil {
		t.Fatalf("building the result anchor: %v", err)
	}

	if _, err := r.f.pool.Exec(ctx, `
		UPDATE ballot_runs
		   SET result_merkle_root = $2, result_cid_digest = $3, status = 'RESULT_ANCHORED'
		 WHERE id = $1`,
		r.ballotRunID, req.Root[:], req.CIDDigest[:],
	); err != nil {
		t.Fatalf("anchoring the result: %v", err)
	}

	resultPayload, err := req.OutboxPayload()
	if err != nil {
		t.Fatal(err)
	}
	resultKey, err := idempotency.Derive(req.AnchorIdempotencyInput(r.f.schemeID, r.f.offerID))
	if err != nil {
		t.Fatal(err)
	}
	r.enqueueAnchor(ctx, "anchorBallotResult", idempotency.ActionFinaliseAllotment,
		resultPayload, resultKey)

	r.ev.Ceremony.ResultAnchor = confirmedAnchor()
	r.ev.AllotmentFilePinned = true
	r.advanceOffer(ctx, offer.StatusBallotDrawn, offer.StatusAllotmentFinalised)
}

// --- stage 6: money and the register ----------------------------------------------------------------

// settleAsba issues one instruction per bid: debit the allotted portion, release the rest.
func (r *primaryRun) settleAsba(ctx context.Context) {
	t := r.t

	for _, l := range r.draw.Lines {
		block := l.Block

		settled, err := r.provider.Settle(ctx, asba.SettleRequest{
			IdempotencyKey: fmt.Sprintf("%x", r.key(idempotency.ActionSettleASBABlock,
				r.f.offerID, map[string]any{"bidRef": l.BidRef, "debit": int64(l.AmountPayable)})),
			BankRef:    block.BankRef,
			DebitPaise: l.AmountPayable,
		})
		if err != nil {
			t.Fatalf("settling bid %s: %v", l.BidID, err)
		}
		if err := settled.Reconcile(); err != nil {
			t.Fatalf("bid %s does not reconcile after settlement: %v", l.BidID, err)
		}

		status := "UNBLOCKED"
		if settled.DebitedPaise > 0 {
			status = "DEBITED"
		}

		if _, err := r.f.pool.Exec(ctx, `
			UPDATE asba_blocks
			   SET block_status = $2, debited_amount_paise = $3, debited_at = $4, unblocked_at = $5
			 WHERE bid_id = $1`,
			l.BidID, status, int64(settled.DebitedPaise), settled.DebitedAt, settled.UnblockedAt,
		); err != nil {
			t.Fatalf("recording the settlement for bid %s: %v", l.BidID, err)
		}

		// # Why a rejected bid keeps its rejected status
		//
		// A losing bid has its money released, so FUNDS_UNBLOCKED looks like the natural next status. It
		// is not, and the schema says so: bids_rejection_reason_present permits a rejection_reason only
		// while the status is REJECTED_BALLOT or REJECTED_TECHNICAL. Advancing a rejected bid to
		// FUNDS_UNBLOCKED would force the reason to be cleared, discarding the record of why the bid
		// lost, which is precisely the thing the bidder is owed an explanation of.
		//
		// So the two facts live where they belong. The bid's status records the ballot outcome and keeps
		// its reason; the block's status records what happened to the money. FUNDS_UNBLOCKED is for a bid
		// released without having been rejected at all, which is what an aborted offer produces.
		if settled.DebitedPaise == 0 {
			continue
		}
		if _, err := r.f.pool.Exec(ctx,
			`UPDATE bids SET status = 'FUNDS_DEBITED' WHERE id = $1`, l.BidID); err != nil {
			t.Fatalf("marking bid %s settled: %v", l.BidID, err)
		}
	}

	// Every rejected bid still says why, and its money is released.
	var orphaned int
	if err := r.f.pool.QueryRow(ctx, `
		SELECT count(*) FROM bids b JOIN asba_blocks a ON a.bid_id = b.id
		 WHERE b.offer_id = $1
		   AND b.status IN ('REJECTED_BALLOT', 'REJECTED_TECHNICAL')
		   AND (b.rejection_reason IS NULL OR a.block_status <> 'UNBLOCKED')`,
		r.f.offerID).Scan(&orphaned); err != nil {
		t.Fatal(err)
	}
	if orphaned != 0 {
		t.Fatalf("%d rejected bids either give no reason or still hold blocked funds", orphaned)
	}
}

// creditRegister builds the settlement plan and applies it to the register.
func (r *primaryRun) creditRegister(ctx context.Context) {
	t := r.t

	wallets := make(map[string]string, len(r.f.investorIDs))
	for i, id := range r.f.investorIDs {
		wallets[id] = r.f.wallets[i]
	}

	plan, err := settlement.BuildPlan(settlement.BuildInput{
		Run:               r.draw,
		Params:            settlement.V1Params(),
		Wallets:           wallets,
		IMWallet:          r.f.imWallet,
		AllotmentFileHash: r.allotPin.Digest,
		BallotResultRoot:  r.draw.Result.ResultRoot,
	})
	if err != nil {
		t.Fatalf("building the settlement plan: %v", err)
	}
	r.plan = plan

	// The manager's units are credited outside the settlement cursor, and before the public side, which
	// is the order the contract's own comment describes.
	r.credit(ctx, r.f.imInvestorID, r.f.imWallet, primaryIMUnits, "IM_SUBSCRIPTION", true)

	// Then the public side, batch by batch, in cursor order.
	var cursor uint32
	for i, b := range plan.Batches {
		if b.CursorFrom != cursor {
			t.Fatalf("batch %d starts at %d, but %d holders are credited", i, b.CursorFrom, cursor)
		}
		for _, e := range b.Entries {
			r.credit(ctx, e.InvestorID, e.Wallet, e.Units, "ALLOTMENT", false)
		}
		cursor = b.CursorTo()
	}

	if cursor != plan.ExpectedHolders {
		t.Fatalf("credited %d holders, declared %d", cursor, plan.ExpectedHolders)
	}

	if _, err := r.f.pool.Exec(ctx,
		`UPDATE bids SET status = 'UNITS_CREDITED'
		  WHERE offer_id = $1 AND status = 'FUNDS_DEBITED'`, r.f.offerID); err != nil {
		t.Fatalf("marking bids credited: %v", err)
	}

	// The manager's units are the one part of the settlement the ballot cannot supply, so the guard is
	// shown refusing before they are recorded and accepting after.
	r.ev.Settlement = offer.Settlement{
		Begun:             true,
		ExpectedHolders:   plan.ExpectedHolders,
		ExpectedUnits:     plan.ExpectedUnits,
		CreditedHolders:   plan.ExpectedHolders,
		CreditedUnits:     plan.ExpectedUnits,
		AllotmentFileHash: r.allotPin.Digest,
		IMUnitsRecorded:   false,
	}
	if err := offer.Guard(offer.StatusAllotmentFinalised, offer.StatusSettled, r.ev); err == nil {
		t.Fatal("settlement must be refused until the manager's subscription is recorded; without " +
			"it the register is short by exactly the manager's holding")
	}

	r.ev.Settlement.IMUnitsRecorded = true
	r.advanceOffer(ctx, offer.StatusAllotmentFinalised, offer.StatusSettled)
}

// credit writes the mirror and the append-only ledger together.
func (r *primaryRun) credit(ctx context.Context, investorID, wallet string, units uint32, entryType string, excluded bool) {
	t := r.t
	t.Helper()

	if _, err := r.f.pool.Exec(ctx, `
		INSERT INTO unit_holdings (
			scheme_id, investor_id, wallet_address, units, is_excluded_from_holder_count,
			first_credited_at
		) VALUES ($1, $2, $3, $4, $5, $6)`,
		r.f.schemeID, investorID, wallet, units, excluded, settledAt,
	); err != nil {
		t.Fatalf("crediting %s: %v", investorID, err)
	}

	// balance_after is checked against the running sum of deltas by trigger, so a first credit must
	// state the balance it produces rather than an arbitrary figure.
	if _, err := r.f.pool.Exec(ctx, `
		INSERT INTO holding_ledger (
			scheme_id, investor_id, entry_type, units_delta, balance_after,
			source, occurred_at, simulated_clock_value, idempotency_key
		) VALUES ($1, $2, $3, $4, $5, 'BALLOT', $6, $6, $7)`,
		r.f.schemeID, investorID, entryType, int32(units), int32(units), settledAt,
		r.key(idempotency.ActionSettleBatch, r.f.offerID,
			map[string]any{"ledger": investorID, "units": int64(units)}),
	); err != nil {
		t.Fatalf("recording the ledger entry for %s: %v", investorID, err)
	}

	if _, err := r.f.pool.Exec(ctx, `
		INSERT INTO depository_register (scheme_id, investor_id, wallet_address, units)
		VALUES ($1, $2, $3, $4)`,
		r.f.schemeID, investorID, wallet, units,
	); err != nil {
		t.Fatalf("seeding the depository for %s: %v", investorID, err)
	}
}

// --- assertions ------------------------------------------------------------------------------------

// assertCapTable checks the register against the five finalisation invariants.
func (r *primaryRun) assertCapTable(ctx context.Context) {
	t := r.t

	var issued, lines, countable int
	if err := r.f.pool.QueryRow(ctx, `
		SELECT coalesce(sum(units), 0), count(*),
		       count(*) FILTER (WHERE units > 0 AND NOT is_excluded_from_holder_count)
		  FROM unit_holdings WHERE scheme_id = $1`, r.f.schemeID).
		Scan(&issued, &lines, &countable); err != nil {
		t.Fatal(err)
	}

	// Check 1: the register totals the scheme's fixed unit count.
	if issued != primaryTotal {
		t.Fatalf("the register holds %d units, the scheme is %d", issued, primaryTotal)
	}

	// The three counts are deliberately different numbers and all three matter.
	wantLines := int(r.plan.ExpectedHolders) + 1 // allottees plus the manager
	if lines != wantLines {
		t.Fatalf("%d register lines, want %d", lines, wantLines)
	}
	if countable != int(r.plan.ExpectedHolders) {
		t.Fatalf("%d countable holders, want %d: the manager must be excluded",
			countable, r.plan.ExpectedHolders)
	}

	// Check 2: the statutory floor.
	if countable < primaryMinHold {
		t.Fatalf("%d countable holders, below the floor of %d", countable, primaryMinHold)
	}

	// Check 3: the manager's holding, excluded from the count.
	var imUnitsHeld int
	var imExcluded bool
	if err := r.f.pool.QueryRow(ctx, `
		SELECT units, is_excluded_from_holder_count FROM unit_holdings
		 WHERE scheme_id = $1 AND investor_id = $2`,
		r.f.schemeID, r.f.imInvestorID).Scan(&imUnitsHeld, &imExcluded); err != nil {
		t.Fatalf("reading the manager's holding: %v", err)
	}
	if imUnitsHeld != primaryIMUnits {
		t.Fatalf("the manager holds %d units, want %d", imUnitsHeld, primaryIMUnits)
	}
	if !imExcluded {
		t.Fatal("the manager must be excluded from the holder count")
	}

	// The public side must be exactly the ballot's allocation, with the manager making up the rest.
	if issued-imUnitsHeld != primaryOnOffer {
		t.Fatalf("the public side holds %d units, want %d", issued-imUnitsHeld, primaryOnOffer)
	}

	// Checks 4 and 5, evaluated exactly as finaliseSettlement would.
	report := settlement.CheckFinalisation(r.plan, settlement.ChainState{
		Stage:               settlement.StageInProgress,
		TotalUnitsIssued:    uint32(issued),
		DistinctHolderCount: uint32(countable),
		IMWallet:            r.f.imWallet,
		IMWalletUnits:       uint32(imUnitsHeld),
		IMExcluded:          imExcluded,
		ExpectedUnits:       r.plan.ExpectedUnits,
		ExpectedHolders:     r.plan.ExpectedHolders,
		CreditedUnits:       r.plan.ExpectedUnits,
		CreditedHolders:     r.plan.ExpectedHolders,
	})
	if !report.Ready {
		t.Fatalf("finaliseSettlement would revert on the register this offer produced: %v",
			report.Failures)
	}

	t.Logf("cap table: %d units across %d register lines, %d countable holders "+
		"(manager holds %d, excluded), %d batches of at most %d",
		issued, lines, countable, imUnitsHeld, len(r.plan.Batches), r.plan.Params.MaxBatchSize)

	// The mirror and the legal register agree, so the scheme starts its first period reconciled.
	mirror := r.readPrimaryPositions(ctx, `
		SELECT investor_id, wallet_address, units FROM unit_holdings
		 WHERE scheme_id = $1 ORDER BY wallet_address`)
	depository := r.readPrimaryPositions(ctx, `
		SELECT investor_id, wallet_address, units FROM depository_register
		 WHERE scheme_id = $1 ORDER BY wallet_address`)

	if len(mirror) != len(depository) {
		t.Fatalf("the mirror has %d lines, the depository %d", len(mirror), len(depository))
	}
	for i := range mirror {
		if mirror[i] != depository[i] {
			t.Fatalf("line %d diverges: mirror %+v, depository %+v", i, mirror[i], depository[i])
		}
	}
}

// assertMoneyReconciles checks the ASBA identity across the whole offer.
func (r *primaryRun) assertMoneyReconciles(ctx context.Context) {
	t := r.t

	var blocked, debited int64
	var unreconciled int
	if err := r.f.pool.QueryRow(ctx, `
		SELECT coalesce(sum(a.blocked_amount_paise), 0),
		       coalesce(sum(a.debited_amount_paise), 0),
		       count(*) FILTER (WHERE a.block_status NOT IN ('DEBITED', 'UNBLOCKED'))
		  FROM asba_blocks a
		  JOIN bids b ON b.id = a.bid_id
		 WHERE b.offer_id = $1`, r.f.offerID).Scan(&blocked, &debited, &unreconciled); err != nil {
		t.Fatal(err)
	}

	if unreconciled != 0 {
		t.Fatalf("%d blocks are neither debited nor released; that money is frozen with no claim "+
			"against it", unreconciled)
	}

	wantBlocked := int64(primaryBidders) * int64(unitsPerBid) * int64(bidPricePaise)
	if blocked != wantBlocked {
		t.Fatalf("%d paise blocked, want %d", blocked, wantBlocked)
	}

	// Only the allotted units are paid for. The rest was released, never ours to hold.
	wantDebited := int64(primaryOnOffer) * int64(bidPricePaise)
	if debited != wantDebited {
		t.Fatalf("%d paise debited for %d units, want %d", debited, primaryOnOffer, wantDebited)
	}

	// The identity, stated across the offer: every paise reserved was either taken for units or
	// handed back.
	released := blocked - debited
	payable, err := r.draw.TotalPayablePaise()
	if err != nil {
		t.Fatal(err)
	}
	refund, err := r.draw.TotalRefundPaise()
	if err != nil {
		t.Fatal(err)
	}
	if int64(payable) != debited {
		t.Fatalf("the draw expects %d debited, the bank recorded %d", payable, debited)
	}
	if int64(refund) != released {
		t.Fatalf("the draw expects %d released, the bank released %d", refund, released)
	}

	// And the money taken equals the cap table at the offer price. Two independent routes to the same
	// figure: one through the bank, one through the register.
	var issuedPublic int64
	if err := r.f.pool.QueryRow(ctx, `
		SELECT coalesce(sum(units), 0) FROM unit_holdings
		 WHERE scheme_id = $1 AND NOT is_excluded_from_holder_count`, r.f.schemeID).
		Scan(&issuedPublic); err != nil {
		t.Fatal(err)
	}
	if issuedPublic*int64(bidPricePaise) != debited {
		t.Fatalf("%d public units at %d paise is %d, but %d was debited",
			issuedPublic, bidPricePaise, issuedPublic*int64(bidPricePaise), debited)
	}

	t.Logf("money: %d paise blocked across %d bids, %d debited for %d units, %d released; "+
		"the draw rejected %d bids",
		blocked, primaryBidders, debited, issuedPublic, released, len(r.draw.Rejected()))
}

// assertPublishedDocumentsVerify walks the verifier's path against the pinned bytes.
func (r *primaryRun) assertPublishedDocumentsVerify(ctx context.Context) {
	t := r.t

	// Both documents must still be retrievable and hash to what was anchored.
	if err := r.publisher.VerifyRoundTrip(ctx, r.bookPin); err != nil {
		t.Fatalf("the bid book must be retrievable and intact: %v", err)
	}
	if err := r.publisher.VerifyRoundTrip(ctx, r.allotPin); err != nil {
		t.Fatalf("the allotment file must be retrievable and intact: %v", err)
	}

	// The anchored digests are of the documents, read back from the row rather than from memory.
	var bookDigest, resultDigest []byte
	if err := r.f.pool.QueryRow(ctx,
		`SELECT bidbook_cid_digest, result_cid_digest FROM ballot_runs WHERE id = $1`,
		r.ballotRunID).Scan(&bookDigest, &resultDigest); err != nil {
		t.Fatal(err)
	}

	gotBook := sha256.Sum256(r.bookPin.CanonicalBytes)
	if string(bookDigest) != string(gotBook[:]) {
		t.Fatalf("the pinned bid book hashes to %x, the anchored digest is %x", gotBook, bookDigest)
	}
	gotResult := sha256.Sum256(r.allotPin.CanonicalBytes)
	if string(resultDigest) != string(gotResult[:]) {
		t.Fatalf("the pinned allotment file hashes to %x, the anchored digest is %x",
			gotResult, resultDigest)
	}

	// An investor proving their own bid was in the book, using their reference and nothing else.
	for _, i := range []int{0, primaryBidders / 2, primaryBidders - 1} {
		line, proof, err := r.book.ProofForBidRef(r.bidRefs[i])
		if err != nil {
			t.Fatalf("looking up bid %d: %v", i, err)
		}
		if !r.book.VerifyLine(line, proof) {
			t.Fatalf("bid %d does not verify against the anchored bid book root", i)
		}
	}
}

func (r *primaryRun) readPrimaryPositions(ctx context.Context, q string) []position {
	r.t.Helper()
	rows, err := r.f.pool.Query(ctx, q, r.f.schemeID)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()

	var out []position
	for rows.Next() {
		var p position
		var units int32
		if err := rows.Scan(&p.investorID, &p.wallet, &units); err != nil {
			r.t.Fatal(err)
		}
		p.units = uint32(units)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// bidReference produces the 32-character lowercase hex form the schema requires.
func bidReference(i int) string {
	d := sha256.Sum256([]byte(fmt.Sprintf("acresync-e2e-bidref-%d", i)))
	return fmt.Sprintf("%x", d[:16])
}

// schemeContract is the deployed AcreSyncScheme address on Sepolia.
const schemeContract = "0xa656a42974b40cf64f32e758abb0689a2a178391"
