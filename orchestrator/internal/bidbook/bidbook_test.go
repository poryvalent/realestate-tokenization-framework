package bidbook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/asba"
	"github.com/acresync/orchestrator/internal/ballot"
	"github.com/acresync/orchestrator/internal/idempotency"
	"github.com/acresync/orchestrator/internal/ipfs"
	"github.com/acresync/orchestrator/internal/ipfsguard"
	"github.com/acresync/orchestrator/internal/merkle"
	"github.com/acresync/orchestrator/internal/money"
)

const (
	testOfferID  = "3f2a1c4e-5b6d-4e8f-9a0b-1c2d3e4f5a6b"
	testSchemeID = "8d7c6b5a-4e3f-4a2b-8c1d-0e9f8a7b6c5d"
)

var frozenAt = time.Date(2026, 4, 1, 9, 30, 0, 0, time.UTC)

func anchorFor(n int) merkle.Hash {
	var h merkle.Hash
	binary.BigEndian.PutUint32(h[:4], uint32(0xA0000000+n))
	h[31] = byte(n)
	return h
}

// bidRefFor produces a deterministic 32-character lowercase hex reference.
//
// Deliberately not ordered the same way as the sequence number: the high bytes vary with n in a way that
// does not sort like n, so a test that assumed leaf index equals input position would notice.
func bidRefFor(n int) string {
	var b [16]byte
	binary.BigEndian.PutUint32(b[:4], uint32(n*2654435761))
	binary.BigEndian.PutUint32(b[12:], uint32(n))
	return hex.EncodeToString(b[:])
}

func blockFor(bidID string, amount money.Paise) *asba.Block {
	at := frozenAt.Add(-24 * time.Hour)
	return &asba.Block{
		BidID:          bidID,
		Status:         asba.StatusBlocked,
		RequestedPaise: amount,
		BlockedPaise:   amount,
		RequestedAt:    at,
		BlockedAt:      &at,
	}
}

func entry(n int, units uint32, price money.Paise) Entry {
	bidID := fmt.Sprintf("bid-%04d", n)
	return Entry{
		BidID:             bidID,
		InvestorID:        fmt.Sprintf("inv-%04d", n),
		BidRef:            bidRefFor(n),
		InvestorAnchor:    anchorFor(n),
		UnitsBid:          units,
		PricePerUnitPaise: price,
		Block:             blockFor(bidID, money.Paise(int64(units))*price),
	}
}

func entries(count int) []Entry {
	out := make([]Entry, 0, count)
	for i := 1; i <= count; i++ {
		units := uint32(i%25) + 1
		out = append(out, entry(i, units, money.MinUnitPricePaise))
	}
	return out
}

func freeze(t *testing.T, es []Entry) *Book {
	t.Helper()
	book, err := Freeze(FreezeInput{
		SchemeRef: anchorFor(0x5c),
		OfferID:   testOfferID,
		FrozenAt:  frozenAt,
		Entries:   es,
	})
	if err != nil {
		t.Fatalf("freezing a valid book: %v", err)
	}
	return book
}

// --- ordering and determinism ---------------------------------------------------------------------

func TestLeafIndicesAreContiguousFromZeroInBidRefOrder(t *testing.T) {
	book := freeze(t, entries(40))

	if len(book.Lines) != 40 {
		t.Fatalf("want 40 lines, got %d", len(book.Lines))
	}

	for i, l := range book.Lines {
		if l.LeafIndex != uint32(i) {
			t.Fatalf("line %d has leaf index %d: ballot.ValidateBook requires indices contiguous "+
				"from zero", i, l.LeafIndex)
		}
		if i > 0 && book.Lines[i-1].BidRef >= l.BidRef {
			t.Fatalf("lines %d and %d are not in ascending bid reference order: %s then %s",
				i-1, i, book.Lines[i-1].BidRef, l.BidRef)
		}
	}
}

// TestRootIsIndependentOfInputOrder is the guard on the whole freeze contract.
//
// The root feeds the seed, so if the database returned rows in a different order and that changed the
// root, the same set of bids would produce a different draw on a re-run.
func TestRootIsIndependentOfInputOrder(t *testing.T) {
	base := entries(50)
	want := freeze(t, base)

	rng := rand.New(rand.NewSource(20260401))
	for attempt := range 8 {
		shuffled := make([]Entry, len(base))
		copy(shuffled, base)
		rng.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})

		got := freeze(t, shuffled)

		if got.MerkleRoot != want.MerkleRoot {
			t.Fatalf("attempt %d: shuffling the input changed the root from %s to %s",
				attempt, want.MerkleRoot.Hex(), got.MerkleRoot.Hex())
		}
		for i := range want.Lines {
			if got.Lines[i].BidRef != want.Lines[i].BidRef {
				t.Fatalf("attempt %d: line %d is bid %s, want %s",
					attempt, i, got.Lines[i].BidRef, want.Lines[i].BidRef)
			}
		}
	}
}

func TestFreezeDoesNotMutateTheCallersSlice(t *testing.T) {
	in := entries(30)

	before := make([]string, len(in))
	for i, e := range in {
		before[i] = e.BidRef
	}

	freeze(t, in)

	for i, e := range in {
		if e.BidRef != before[i] {
			t.Fatalf("position %d changed from %s to %s: Freeze reordered the caller's slice",
				i, before[i], e.BidRef)
		}
	}
}

// --- funding -------------------------------------------------------------------------------------

func TestBidWithoutABlockIsRefused(t *testing.T) {
	es := entries(5)
	es[2].Block = nil

	_, err := Freeze(FreezeInput{OfferID: testOfferID, FrozenAt: frozenAt, Entries: es})
	if !errors.Is(err, ErrUnfunded) {
		t.Fatalf("want ErrUnfunded, got %v", err)
	}
}

func TestBidWhoseBlockIsNotHoldingFundsIsRefused(t *testing.T) {
	for _, st := range []asba.BlockStatus{
		asba.StatusRequested,
		asba.StatusFailed,
		asba.StatusUnblocked,
		asba.StatusDebited,
	} {
		t.Run(string(st), func(t *testing.T) {
			es := entries(5)
			es[1].Block.Status = st

			_, err := Freeze(FreezeInput{OfferID: testOfferID, FrozenAt: frozenAt, Entries: es})
			if err == nil {
				t.Fatalf("a bid whose block is %s must not enter the book: only BLOCKED holds funds", st)
			}
		})
	}
}

// TestBlockCoveringTheWrongAmountIsRefused is the check that a stale block cannot slip through.
//
// A block placed before a correction to units or price is still BLOCKED, and still looks fine to a
// presence check, while covering the wrong amount. That bid would be admitted underfunded by exactly the
// difference, and the shortfall would surface at debit time with the cap table already anchored.
func TestBlockCoveringTheWrongAmountIsRefused(t *testing.T) {
	t.Run("units raised after the block", func(t *testing.T) {
		es := entries(5)
		es[3].UnitsBid += 1 // block still covers the old count

		_, err := Freeze(FreezeInput{OfferID: testOfferID, FrozenAt: frozenAt, Entries: es})
		if !errors.Is(err, ErrAmountMismatch) {
			t.Fatalf("want ErrAmountMismatch, got %v", err)
		}
	})

	t.Run("block is larger than the bid", func(t *testing.T) {
		es := entries(5)
		es[3].Block.RequestedPaise += money.MinUnitPricePaise
		es[3].Block.BlockedPaise += money.MinUnitPricePaise

		_, err := Freeze(FreezeInput{OfferID: testOfferID, FrozenAt: frozenAt, Entries: es})
		if !errors.Is(err, ErrAmountMismatch) {
			t.Fatalf("an over-block is as wrong as an under-block: want ErrAmountMismatch, got %v", err)
		}
	})
}

func TestBlockFailingItsOwnReconciliationIsRefused(t *testing.T) {
	es := entries(5)
	// BLOCKED with no timestamp: rejected by asba.Block.Reconcile and by the asba_blocked_has_timestamp
	// CHECK. Freeze delegates rather than reimplementing the rule.
	es[0].Block.BlockedAt = nil

	_, err := Freeze(FreezeInput{OfferID: testOfferID, FrozenAt: frozenAt, Entries: es})
	if err == nil {
		t.Fatal("a block that fails its own reconciliation must not be admitted to the book")
	}
}

// --- uniqueness ----------------------------------------------------------------------------------

func TestDuplicateInvestorIsRefused(t *testing.T) {
	es := entries(5)
	es[4].InvestorID = es[0].InvestorID

	_, err := Freeze(FreezeInput{OfferID: testOfferID, FrozenAt: frozenAt, Entries: es})
	if !errors.Is(err, ErrDuplicateInvestor) {
		t.Fatalf("want ErrDuplicateInvestor, got %v", err)
	}
}

func TestDuplicateBidRefIsRefused(t *testing.T) {
	es := entries(5)
	es[4].BidRef = es[0].BidRef

	_, err := Freeze(FreezeInput{OfferID: testOfferID, FrozenAt: frozenAt, Entries: es})
	if !errors.Is(err, ErrDuplicateBidRef) {
		t.Fatalf("want ErrDuplicateBidRef, got %v", err)
	}
}

func TestDuplicateAnchorIsRefused(t *testing.T) {
	es := entries(5)
	es[4].InvestorAnchor = es[0].InvestorAnchor

	_, err := Freeze(FreezeInput{OfferID: testOfferID, FrozenAt: frozenAt, Entries: es})
	if !errors.Is(err, ErrDuplicateAnchor) {
		t.Fatalf("want ErrDuplicateAnchor, got %v", err)
	}
}

func TestEmptyBookIsRefused(t *testing.T) {
	_, err := Freeze(FreezeInput{OfferID: testOfferID, FrozenAt: frozenAt})
	if !errors.Is(err, ErrEmptyBook) {
		t.Fatalf("want ErrEmptyBook, got %v", err)
	}
}

// --- counts --------------------------------------------------------------------------------------

func TestLeafCountEqualsDistinctBidders(t *testing.T) {
	book := freeze(t, entries(60))

	if book.LeafCount != book.DistinctBidders {
		t.Fatalf("leaf count %d against %d distinct bidders: one bid per investor is enforced by the "+
			"schema, so these cannot legitimately differ", book.LeafCount, book.DistinctBidders)
	}
	if book.LeafCount != 60 {
		t.Fatalf("want 60 leaves, got %d", book.LeafCount)
	}
}

func TestTotalsAgreeWithTheLines(t *testing.T) {
	es := entries(45)
	book := freeze(t, es)

	var units uint64
	var amount money.Paise
	for _, l := range book.Lines {
		units += uint64(l.UnitsBid)
		amount += money.Paise(int64(l.UnitsBid)) * l.PricePerUnitPaise
	}

	if uint64(book.TotalUnitsBid) != units {
		t.Fatalf("TotalUnitsBid is %d, lines sum to %d", book.TotalUnitsBid, units)
	}
	if book.TotalAmountPaise != amount {
		t.Fatalf("TotalAmountPaise is %d, lines sum to %d", book.TotalAmountPaise, amount)
	}
}

// TestTotalAmountEqualsTotalBlocked ties the book to the money reserved behind it.
func TestTotalAmountEqualsTotalBlocked(t *testing.T) {
	book := freeze(t, entries(45))

	var blocked money.Paise
	for _, l := range book.Lines {
		blocked += l.Block.BlockedPaise
	}

	if book.TotalAmountPaise != blocked {
		t.Fatalf("the book commits %d paise but %d is blocked", book.TotalAmountPaise, blocked)
	}
}

// --- proofs --------------------------------------------------------------------------------------

func TestProofForEveryLineVerifiesAgainstTheRoot(t *testing.T) {
	book := freeze(t, entries(37)) // not a power of two, so the tree is ragged

	for _, l := range book.Lines {
		proof, err := book.Proof(l.LeafIndex)
		if err != nil {
			t.Fatalf("leaf %d: %v", l.LeafIndex, err)
		}
		if !book.VerifyLine(l, proof) {
			t.Fatalf("leaf %d does not verify against the root", l.LeafIndex)
		}
	}
}

func TestProofByBidRefRoundTrips(t *testing.T) {
	es := entries(37)
	book := freeze(t, es)

	for _, e := range es {
		line, proof, err := book.ProofForBidRef(e.BidRef)
		if err != nil {
			t.Fatalf("looking up %s: %v", e.BidRef, err)
		}
		if line.BidID != e.BidID {
			t.Fatalf("reference %s resolved to bid %s, want %s", e.BidRef, line.BidID, e.BidID)
		}
		if !book.VerifyLine(line, proof) {
			t.Fatalf("the proof for %s does not verify", e.BidRef)
		}
	}
}

func TestUnknownBidRefIsNotFound(t *testing.T) {
	book := freeze(t, entries(10))
	if _, _, err := book.ProofForBidRef(bidRefFor(9999)); err == nil {
		t.Fatal("a reference that is not in the book must not resolve")
	}
}

// TestTamperedLineFailsVerification is why VerifyLine recomputes the leaf.
//
// Comparing a stored leaf against a root built from those same stored leaves proves nothing. The
// recomputation is what an external verifier does, and it is what catches an altered unit count.
func TestTamperedLineFailsVerification(t *testing.T) {
	book := freeze(t, entries(20))

	line, proof, err := book.ProofForBidRef(book.Lines[7].BidRef)
	if err != nil {
		t.Fatal(err)
	}
	if !book.VerifyLine(line, proof) {
		t.Fatal("the untampered line must verify first")
	}

	for name, mutate := range map[string]func(*Line){
		"units raised":    func(l *Line) { l.UnitsBid += 1 },
		"price raised":    func(l *Line) { l.PricePerUnitPaise += 1 },
		"anchor swapped":  func(l *Line) { l.InvestorAnchor = anchorFor(4242) },
		"index moved":     func(l *Line) { l.LeafIndex = 8 },
		"reference moved": func(l *Line) { l.BidRef = bidRefFor(5555) },
	} {
		t.Run(name, func(t *testing.T) {
			tampered := line
			mutate(&tampered)
			if book.VerifyLine(tampered, proof) {
				t.Fatalf("a line with %s must not verify against the frozen root", name)
			}
		})
	}
}

// --- agreement with the allocation engine ---------------------------------------------------------

// TestRootMatchesTheAllocationEngine checks the two constructions agree.
//
// Freeze delegates the root to ballot.BidbookRoot and separately builds a local tree for proofs. If
// those diverged, proofs would fail for investors while the anchored root looked fine.
func TestRootMatchesTheAllocationEngine(t *testing.T) {
	book := freeze(t, entries(33))

	engine, err := ballot.BidbookRoot(book.BallotBids())
	if err != nil {
		t.Fatal(err)
	}
	if engine != book.MerkleRoot {
		t.Fatalf("the engine computes %s, the book holds %s", engine.Hex(), book.MerkleRoot.Hex())
	}
}

func TestBallotBidsPassValidateBook(t *testing.T) {
	book := freeze(t, entries(33))
	if err := ballot.ValidateBook(book.BallotBids()); err != nil {
		t.Fatalf("a frozen book must satisfy the allocation engine's own validation: %v", err)
	}
}

func TestBookCanBeAllocated(t *testing.T) {
	// 500 bidders for 475 units: oversubscribed, so the draw actually has work to do.
	book := freeze(t, entries(500))

	params := ballot.V1Params()

	seed := anchorFor(0x5eed)
	result, err := ballot.Run(book.BallotBids(), params, seed)
	if err != nil {
		t.Fatalf("running the draw over a frozen book: %v", err)
	}

	if result.BidbookRoot != book.MerkleRoot {
		t.Fatalf("the result is bound to root %s, the book is %s",
			result.BidbookRoot.Hex(), book.MerkleRoot.Hex())
	}

	// Every bid gets an allocation, including the losing ones.
	if len(result.Allocations) != len(book.Lines) {
		t.Fatalf("%d allocations for %d bids: a losing bidder needs a published outcome too",
			len(result.Allocations), len(book.Lines))
	}

	for _, a := range result.Allocations {
		if _, ok := book.BidIDForLeaf(a.LeafIndex); !ok {
			t.Fatalf("allocation references leaf %d, which is not in the book", a.LeafIndex)
		}
	}
}

func TestBidIDForLeafRejectsAnIndexPastTheEnd(t *testing.T) {
	book := freeze(t, entries(10))
	if _, ok := book.BidIDForLeaf(10); ok {
		t.Fatal("leaf 10 does not exist in a book of 10 leaves")
	}
}

func TestOversubscriptionRatioIsExact(t *testing.T) {
	book := freeze(t, entries(500))
	num, den := book.OversubscriptionRatio(475)

	if num != book.TotalUnitsBid || den != 475 {
		t.Fatalf("want %d/475, got %d/%d", book.TotalUnitsBid, num, den)
	}
}

// --- document ------------------------------------------------------------------------------------

func TestDocumentSatisfiesTheAllowlist(t *testing.T) {
	book := freeze(t, entries(120))

	raw, err := book.Document()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ipfsguard.ValidateAndCanonicalize(ipfsguard.DocBidbook, raw); err != nil {
		t.Fatalf("the bid book document must satisfy the allowlist: %v", err)
	}
}

func TestDocumentIsCanonicallyStable(t *testing.T) {
	book := freeze(t, entries(60))

	first, err := book.Document()
	if err != nil {
		t.Fatal(err)
	}
	a, err := ipfsguard.ValidateAndCanonicalize(ipfsguard.DocBidbook, first)
	if err != nil {
		t.Fatal(err)
	}

	second, err := book.Document()
	if err != nil {
		t.Fatal(err)
	}
	b, err := ipfsguard.ValidateAndCanonicalize(ipfsguard.DocBidbook, second)
	if err != nil {
		t.Fatal(err)
	}

	if string(a) != string(b) {
		t.Fatal("two renderings of one book produced different canonical bytes, so the book would " +
			"have two digests to anchor")
	}
}

func TestDocumentCarriesEveryLineAndNoIdentifiers(t *testing.T) {
	book := freeze(t, entries(25))

	raw, err := book.Document()
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		OfferID    string `json:"offerId"`
		FrozenAt   string `json:"frozenAt"`
		MerkleRoot string `json:"merkleRoot"`
		LeafCount  uint32 `json:"leafCount"`
		TotalUnits uint32 `json:"totalUnitsBid"`
		Distinct   uint32 `json:"distinctBidders"`
		AlgoVer    uint32 `json:"algoVersion"`
		Leaves     []struct {
			LeafIndex uint32 `json:"leafIndex"`
			BidRef    string `json:"bidRef"`
			Anchor    string `json:"investorAnchor"`
			Units     uint32 `json:"unitsBid"`
			Price     uint64 `json:"pricePerUnitPaise"`
		} `json:"leaves"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	if len(doc.Leaves) != 25 {
		t.Fatalf("want 25 published leaves, got %d", len(doc.Leaves))
	}
	if doc.MerkleRoot != book.MerkleRoot.Hex() {
		t.Fatalf("published root %s, book root %s", doc.MerkleRoot, book.MerkleRoot.Hex())
	}
	if doc.FrozenAt != "2026-04-01T09:30:00Z" {
		t.Fatalf("frozenAt must be UTC with a Z suffix, got %q", doc.FrozenAt)
	}
	if doc.AlgoVer != ballot.AlgoVersion {
		t.Fatalf("published algoVersion %d, engine %d", doc.AlgoVer, ballot.AlgoVersion)
	}
	if doc.LeafCount != 25 || doc.Distinct != 25 {
		t.Fatalf("leafCount %d and distinctBidders %d, want 25 and 25", doc.LeafCount, doc.Distinct)
	}

	// The internal identifiers must not appear anywhere in the bytes.
	for _, e := range entries(25) {
		if bytesContain(raw, e.InvestorID) {
			t.Fatalf("the document contains investor identifier %s", e.InvestorID)
		}
		if bytesContain(raw, e.BidID) {
			t.Fatalf("the document contains internal bid identifier %s", e.BidID)
		}
	}
}

// TestDocumentLeavesAreRecomputableFromWhatIsPublished is the verifier's path, end to end.
//
// Nothing here touches the Book's stored leaves or its tree: the leaves are rebuilt from the published
// fields alone and the root is reconstructed from those. That is the whole claim the pinned document
// makes, so it is worth exercising literally.
func TestDocumentLeavesAreRecomputableFromWhatIsPublished(t *testing.T) {
	book := freeze(t, entries(37))

	raw, err := book.Document()
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		MerkleRoot string `json:"merkleRoot"`
		Leaves     []struct {
			LeafIndex uint32 `json:"leafIndex"`
			BidRef    string `json:"bidRef"`
			Anchor    string `json:"investorAnchor"`
			Units     uint32 `json:"unitsBid"`
			Price     uint64 `json:"pricePerUnitPaise"`
		} `json:"leaves"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	rebuilt := make([]merkle.Hash, 0, len(doc.Leaves))
	for _, l := range doc.Leaves {
		anchor, err := merkle.ParseHash(l.Anchor)
		if err != nil {
			t.Fatalf("published anchor %q does not parse: %v", l.Anchor, err)
		}
		leaf, err := ballot.BidLeaf(ballot.Bid{
			LeafIndex:         l.LeafIndex,
			BidRef:            l.BidRef,
			InvestorAnchor:    anchor,
			UnitsBid:          l.Units,
			PricePerUnitPaise: money.Paise(l.Price),
		})
		if err != nil {
			t.Fatal(err)
		}
		rebuilt = append(rebuilt, leaf)
	}

	tree, err := merkle.New(rebuilt)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Root().Hex() != doc.MerkleRoot {
		t.Fatalf("a reader rebuilding the tree from the published lines gets %s, the document claims %s",
			tree.Root().Hex(), doc.MerkleRoot)
	}
	if tree.Root() != book.MerkleRoot {
		t.Fatalf("the rebuilt root disagrees with the book's own root")
	}
}

func TestDocumentRefusesANonCanonicalOfferID(t *testing.T) {
	for _, id := range []string{
		"",
		"not-a-uuid",
		"3F2A1C4E-5B6D-4E8F-9A0B-1C2D3E4F5A6B", // uppercase
		"3f2a1c4e5b6d4e8f9a0b1c2d3e4f5a6b",     // unhyphenated
	} {
		book := freeze(t, entries(3))
		book.OfferID = id

		if _, err := book.Document(); err == nil {
			t.Fatalf("offerId %q must be refused before the allowlist sees it", id)
		}
	}
}

func bytesContain(haystack []byte, needle string) bool {
	return needle != "" && bytes.Contains(haystack, []byte(needle))
}

// --- pin and anchor ------------------------------------------------------------------------------

func pinBook(t *testing.T, book *Book) (*ipfs.Pin, *ipfs.MockProvider) {
	t.Helper()

	mock, err := ipfs.NewMockProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pub := ipfs.NewPublisher(mock)

	raw, err := book.Document()
	if err != nil {
		t.Fatal(err)
	}
	pin, err := pub.Publish(context.Background(), ipfsguard.DocBidbook, raw)
	if err != nil {
		t.Fatalf("pinning the bid book: %v", err)
	}
	return pin, mock
}

// TestPinnedDocumentIsRetrievableAndIntact exercises the publisher's own round-trip check.
func TestPinnedDocumentIsRetrievableAndIntact(t *testing.T) {
	book := freeze(t, entries(90))

	mock, err := ipfs.NewMockProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pub := ipfs.NewPublisher(mock)

	raw, err := book.Document()
	if err != nil {
		t.Fatal(err)
	}
	pin, err := pub.Publish(context.Background(), ipfsguard.DocBidbook, raw)
	if err != nil {
		t.Fatal(err)
	}

	if err := pub.VerifyRoundTrip(context.Background(), pin); err != nil {
		t.Fatalf("a pinned bid book must be retrievable and hash to its anchored digest: %v", err)
	}
}

func TestAnchorRequestCarriesWhatTheContractChecks(t *testing.T) {
	book := freeze(t, entries(300))
	pin, _ := pinBook(t, book)

	req, err := book.AnchorRequest(pin)
	if err != nil {
		t.Fatalf("building the anchor request: %v", err)
	}

	if req.Root != book.MerkleRoot {
		t.Fatalf("anchoring root %s, book root %s", req.Root.Hex(), book.MerkleRoot.Hex())
	}
	if req.LeafCount != book.LeafCount || req.TotalUnitsBid != book.TotalUnitsBid ||
		req.DistinctBidders != book.DistinctBidders {
		t.Fatalf("counts disagree with the book: %d/%d/%d against %d/%d/%d",
			req.LeafCount, req.TotalUnitsBid, req.DistinctBidders,
			book.LeafCount, book.TotalUnitsBid, book.DistinctBidders)
	}

	// anchorBidbook reverts with InvalidCounts when bidders exceed leaves.
	if req.DistinctBidders > req.LeafCount {
		t.Fatal("anchorBidbook would revert with InvalidCounts")
	}
	if req.Root.IsZero() || req.CIDDigest == ([32]byte{}) {
		t.Fatal("anchorBidbook reverts with ZeroValue on a zero root or digest")
	}
}

// TestAnchoredDigestIsOfTheDocumentNotTheCID is the commitment rule, asserted literally.
//
// A verifier fetches the bytes, hashes them, and compares. That has to work without trusting our CID
// derivation, because a locator that stops resolving must not take the commitment with it.
func TestAnchoredDigestIsOfTheDocumentNotTheCID(t *testing.T) {
	book := freeze(t, entries(80))
	pin, _ := pinBook(t, book)

	req, err := book.AnchorRequest(pin)
	if err != nil {
		t.Fatal(err)
	}

	if req.CIDDigest != sha256.Sum256(pin.CanonicalBytes) {
		t.Fatal("the anchored digest must be sha256 of the canonical document bytes")
	}
	if req.CID == "" {
		t.Fatal("the CID should still be recorded as a locator")
	}
}

// TestAStalePinIsRefused is the failure this whole check exists for.
//
// Freeze, pin, correct a bid, re-freeze, then anchor with the pin still in hand from the first attempt.
// Every individual value looks valid. The result would be a permanent anchor whose published document
// does not produce the anchored root, discovered by whoever first tries to reproduce it.
func TestAStalePinIsRefused(t *testing.T) {
	first := freeze(t, entries(20))
	stalePin, _ := pinBook(t, first)

	corrected := entries(20)
	corrected[5].UnitsBid += 3
	corrected[5].Block = blockFor(corrected[5].BidID,
		money.Paise(int64(corrected[5].UnitsBid))*corrected[5].PricePerUnitPaise)
	second := freeze(t, corrected)

	if second.MerkleRoot == first.MerkleRoot {
		t.Fatal("the fixture is wrong: correcting a bid must change the root")
	}

	if _, err := second.AnchorRequest(stalePin); !errors.Is(err, ErrPinDoesNotBind) {
		t.Fatalf("anchoring a re-frozen book with the earlier pin must be refused, got %v", err)
	}

	// The original book still anchors against its own pin.
	if _, err := first.AnchorRequest(stalePin); err != nil {
		t.Fatalf("the pin must still bind to the book it was taken of: %v", err)
	}
}

func TestAnchorRefusesAPinOfAnotherDocumentType(t *testing.T) {
	book := freeze(t, entries(10))

	pin, _ := pinBook(t, book)
	pin.DocType = ipfsguard.DocSnapshot

	if _, err := book.AnchorRequest(pin); !errors.Is(err, ErrWrongDocType) {
		t.Fatalf("want ErrWrongDocType, got %v", err)
	}
}

func TestAnchorRefusesAMissingPin(t *testing.T) {
	book := freeze(t, entries(10))
	if _, err := book.AnchorRequest(nil); !errors.Is(err, ErrNoPin) {
		t.Fatalf("want ErrNoPin, got %v", err)
	}
}

func TestAnchorRefusesABookMutatedAfterFreezing(t *testing.T) {
	book := freeze(t, entries(10))
	pin, _ := pinBook(t, book)

	// Counts tampered with directly, as a corrupted in-memory book or a bad database read would look.
	// The pin no longer matches, because the document embeds the counts.
	book.DistinctBidders = book.LeafCount + 1

	if _, err := book.AnchorRequest(pin); err == nil {
		t.Fatal("a book whose counts were altered after freezing must not anchor")
	}
}

func TestPinnedDocumentRoundTripsAndVerifies(t *testing.T) {
	book := freeze(t, entries(150))
	pin, _ := pinBook(t, book)

	if !pin.SingleBlock {
		t.Fatalf("a %d-byte document should fit a single raw block", pin.ByteSize)
	}
	if !pin.CodecVerified {
		t.Error("the mock stores raw blocks, so the derived address must be the provider's")
	}

	// The fetched bytes must hash to the anchored digest.
	req, err := book.AnchorRequest(pin)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(pin.CanonicalBytes) != req.CIDDigest {
		t.Fatal("the stored bytes do not hash to the anchored digest")
	}
}

// --- idempotency ---------------------------------------------------------------------------------

func TestAnchorKeyIsStableForTheSameBook(t *testing.T) {
	book := freeze(t, entries(40))
	book.SchemeID = testSchemeID
	pin, _ := pinBook(t, book)

	req, err := book.AnchorRequest(pin)
	if err != nil {
		t.Fatal(err)
	}

	k1, err := book.IdempotencyKey(req)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := book.IdempotencyKey(req)
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatal("a retry of the same anchor must derive the same key, or a dropped connection " +
			"double-anchors")
	}
}

func TestADifferentBookDerivesADifferentAnchorKey(t *testing.T) {
	a := freeze(t, entries(40))
	a.SchemeID = testSchemeID
	pinA, _ := pinBook(t, a)
	reqA, err := a.AnchorRequest(pinA)
	if err != nil {
		t.Fatal(err)
	}
	keyA, err := a.IdempotencyKey(reqA)
	if err != nil {
		t.Fatal(err)
	}

	changed := entries(40)
	changed[11].UnitsBid += 1
	changed[11].Block = blockFor(changed[11].BidID,
		money.Paise(int64(changed[11].UnitsBid))*changed[11].PricePerUnitPaise)
	b := freeze(t, changed)
	b.SchemeID = testSchemeID
	pinB, _ := pinBook(t, b)
	reqB, err := b.AnchorRequest(pinB)
	if err != nil {
		t.Fatal(err)
	}
	keyB, err := b.IdempotencyKey(reqB)
	if err != nil {
		t.Fatal(err)
	}

	if keyA == keyB {
		t.Fatal("a changed book must derive a different key, or the correction would be swallowed " +
			"as a duplicate")
	}
}

// TestFreezeAndAnchorKeysDiffer keeps a failed anchor retryable.
//
// One key covering both acts would mean a successful freeze followed by a failed anchor could never be
// retried: the retry would derive a key already marked used.
func TestFreezeAndAnchorKeysDiffer(t *testing.T) {
	book := freeze(t, entries(40))
	book.SchemeID = testSchemeID
	pin, _ := pinBook(t, book)

	req, err := book.AnchorRequest(pin)
	if err != nil {
		t.Fatal(err)
	}

	anchorKey, err := book.IdempotencyKey(req)
	if err != nil {
		t.Fatal(err)
	}
	freezeKey, err := idempotency.Derive(book.FreezeIdempotencyInput())
	if err != nil {
		t.Fatal(err)
	}

	if anchorKey == freezeKey {
		t.Fatal("freezing and anchoring must not share a key")
	}
}

func TestOutboxPayloadCarriesTheCallArguments(t *testing.T) {
	book := freeze(t, entries(30))
	pin, _ := pinBook(t, book)

	req, err := book.AnchorRequest(pin)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := req.OutboxPayload()
	if err != nil {
		t.Fatal(err)
	}

	var got struct {
		Root      []byte `json:"root"`
		CIDDigest []byte `json:"cidDigest"`
		LeafCount uint32 `json:"leafCount"`
		UnitsBid  uint32 `json:"unitsBid"`
		Bidders   uint32 `json:"bidders"`
		IPFSCid   string `json:"ipfsCid"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got.Root, book.MerkleRoot[:]) {
		t.Fatalf("payload root %x, book root %x", got.Root, book.MerkleRoot[:])
	}
	if !bytes.Equal(got.CIDDigest, req.CIDDigest[:]) {
		t.Fatalf("payload digest %x, want %x", got.CIDDigest, req.CIDDigest[:])
	}
	if got.LeafCount != 30 || got.Bidders != 30 {
		t.Fatalf("payload counts %d/%d, want 30/30", got.LeafCount, got.Bidders)
	}
	if got.IPFSCid != req.CID {
		t.Fatalf("payload CID %q, want %q", got.IPFSCid, req.CID)
	}
}
