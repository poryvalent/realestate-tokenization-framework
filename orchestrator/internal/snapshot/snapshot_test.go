package snapshot

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/acresync/orchestrator/internal/ipfsguard"
	"github.com/acresync/orchestrator/internal/merkle"
)

var (
	recordDate = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	periodEnd  = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	takenAt    = time.Date(2026, 9, 30, 18, 30, 0, 0, time.UTC)
)

func anchor(b byte) [32]byte {
	var h [32]byte
	for i := range h {
		h[i] = b
	}
	return h
}

func wallet(n int) string { return fmt.Sprintf("0x%040x", n) }

func holding(n int, units uint32, excluded bool) Holding {
	return Holding{
		InvestorID:              fmt.Sprintf("%08d-0000-4000-8000-000000000000", n),
		WalletAddress:           wallet(n),
		InvestorAnchor:          anchor(byte(n)),
		Units:                   units,
		ExcludedFromHolderCount: excluded,
	}
}

func baseInput(holdings []Holding) BuildInput {
	return BuildInput{
		SchemeRef:  anchor(0x5c),
		PeriodID:   1,
		RecordDate: recordDate,
		PeriodEnd:  periodEnd,
		TakenAt:    takenAt,
		Holdings:   holdings,
	}
}

// liveRegister models the real v1 shape: 500 units, the manager holding 25 excluded from the count,
// and enough public holders to clear the statutory minimum.
func liveRegister() []Holding {
	out := []Holding{{
		InvestorID:              "00000001-0000-4000-8000-000000000000",
		WalletAddress:           wallet(1),
		InvestorAnchor:          anchor(1),
		Units:                   25,
		ExcludedFromHolderCount: true,
	}}

	// 201 public holders sharing 475 units: 200 holders with 2 units and one with 75.
	for i := 0; i < 200; i++ {
		h := holding(i+2, 2, false)
		out = append(out, h)
	}
	last := holding(500, 75, false)
	out = append(out, last)

	return out
}

// ---------------------------------------------------------------------------
// The trap in the excluded flag
// ---------------------------------------------------------------------------

// TestDenominatorIncludesExcludedUnits is the highest-value test in this package.
//
// "Excluded" means excluded from the statutory holder count, not from the distribution. The manager's
// 25 units do not help satisfy the 200-unitholder requirement, yet the manager is entitled to
// distributions on them, so the entitlement denominator is every issued unit.
//
// Reading it the other way divides the distributable amount across 475 units instead of 500, which
// overpays every public holder by about 5% and leaves the manager unpaid. The resulting figure still
// satisfies the 95% floor and reconciles against itself, so nothing downstream would catch it. The
// only thing standing between that and production is this assertion.
func TestDenominatorIncludesExcludedUnits(t *testing.T) {
	snap, err := Build(baseInput(liveRegister()))
	if err != nil {
		t.Fatal(err)
	}

	if snap.TotalUnits != 500 {
		t.Fatalf("total units = %d, want 500", snap.TotalUnits)
	}
	if snap.DistinctHolders != 201 {
		t.Fatalf("countable holders = %d, want 201 (the manager is excluded)", snap.DistinctHolders)
	}

	if got := snap.EntitlementDenominator(); got != 500 {
		t.Fatalf("entitlement denominator = %d, want 500. Using 475 would overpay every public "+
			"holder by about 5%% and pay the manager nothing, while still satisfying the 95%% floor "+
			"and reconciling against itself", got)
	}

	// And the excluded line is present in the register with its units intact.
	var found bool
	for _, l := range snap.Lines {
		if l.ExcludedFromHolderCount {
			found = true
			if l.Units != 25 {
				t.Errorf("the excluded line holds %d units, want 25", l.Units)
			}
		}
	}
	if !found {
		t.Fatal("the excluded holder must still appear on the register")
	}

	// The countable count is the one measured against the statutory minimum, not the line count.
	if uint32(len(snap.Lines)) == snap.DistinctHolders {
		t.Error("line count and countable holder count must differ when a holder is excluded")
	}
	if !snap.MeetsHolderMinimum() {
		t.Error("201 countable holders clears the minimum of 200")
	}
}

func TestHolderMinimumEnforced(t *testing.T) {
	// 199 countable holders plus the excluded manager.
	holdings := []Holding{{
		InvestorID: "00000001-0000-4000-8000-000000000000", WalletAddress: wallet(1),
		InvestorAnchor: anchor(1), Units: 25, ExcludedFromHolderCount: true,
	}}
	for i := 0; i < 199; i++ {
		holdings = append(holdings, holding(i+2, 1, false))
	}

	in := baseInput(holdings)
	in.RequireMinimumHolders = true

	_, err := Build(in)
	if !errors.Is(err, ErrTooFewHolders) {
		t.Fatalf("want ErrTooFewHolders, got %v", err)
	}
	if !strings.Contains(err.Error(), "199") {
		t.Errorf("the error should state the actual count, got %v", err)
	}
}

// TestExcludedHolderDoesNotCountTowardMinimum covers the specific gaming route.
func TestExcludedHolderDoesNotCountTowardMinimum(t *testing.T) {
	holdings := []Holding{}
	for i := 0; i < 200; i++ {
		holdings = append(holdings, holding(i+1, 1, false))
	}
	// The 200th countable holder is switched to excluded, dropping the count to 199.
	holdings[199].ExcludedFromHolderCount = true

	in := baseInput(holdings)
	in.RequireMinimumHolders = true

	if _, err := Build(in); !errors.Is(err, ErrTooFewHolders) {
		t.Fatalf("an excluded holder must not count toward the minimum, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Leaf encoding
// ---------------------------------------------------------------------------

// TestLeafPreimageLayout pins the byte layout down explicitly rather than trusting the helper.
//
// Constructed independently here so a change to EntitlementLeaf cannot quietly change the layout and
// still pass. This layout is what an external verifier must reproduce, and no on-chain function
// validates it: verifyEntitlement accepts whatever leaf it is handed, so a divergence fails silently.
func TestLeafPreimageLayout(t *testing.T) {
	addr, err := ParseAddress("0x000000000000000000000000000000000000dead")
	if err != nil {
		t.Fatal(err)
	}
	a := anchor(0xa7)

	got := LeafPreimage(3, addr, a, 25, false)

	var want []byte
	want = append(want, LeafDomain...)
	want = binary.BigEndian.AppendUint32(want, 3)
	want = append(want, addr[:]...)
	want = append(want, a[:]...)
	want = binary.BigEndian.AppendUint32(want, 25)
	want = append(want, 0x00)

	if string(got) != string(want) {
		t.Fatalf("preimage layout:\n got %x\nwant %x", got, want)
	}

	// 32 bytes of domain, 4 of index, 20 of address, 32 of anchor, 4 of units, 1 flag.
	if len(got) != 32+4+20+32+4+1 {
		t.Errorf("preimage is %d bytes, want 93", len(got))
	}

	// And the digest is the domain-separated leaf hash, not a bare sha256.
	if EntitlementLeaf(3, addr, a, 25, false) != merkle.HashLeaf(want) {
		t.Error("the leaf must be merkle.HashLeaf of the preimage, so a node cannot be replayed as a leaf")
	}
	if EntitlementLeaf(3, addr, a, 25, false) == merkle.Hash(sha256.Sum256(want)) {
		t.Error("the leaf must not be a bare sha256 of the preimage; the 0x00 prefix is what " +
			"separates leaves from nodes")
	}
}

// TestLeafIsInjective covers the collision a variable-length encoding would permit.
func TestLeafIsInjective(t *testing.T) {
	addr, _ := ParseAddress(wallet(1))
	a := anchor(1)

	seen := map[merkle.Hash]string{}
	for idx := uint32(0); idx < 40; idx++ {
		for units := uint32(1); units < 40; units++ {
			for _, excl := range []bool{false, true} {
				leaf := EntitlementLeaf(idx, addr, a, units, excl)
				key := fmt.Sprintf("idx=%d units=%d excl=%v", idx, units, excl)
				if prev, dup := seen[leaf]; dup {
					t.Fatalf("collision between %s and %s", prev, key)
				}
				seen[leaf] = key
			}
		}
	}
}

// TestExcludedFlagIsBoundIntoTheLeaf guards the holder count from being restated after anchoring.
func TestExcludedFlagIsBoundIntoTheLeaf(t *testing.T) {
	addr, _ := ParseAddress(wallet(1))
	a := anchor(1)

	if EntitlementLeaf(0, addr, a, 5, false) == EntitlementLeaf(0, addr, a, 5, true) {
		t.Fatal("flipping the excluded flag must change the leaf, or a holder could be reclassified " +
			"as countable after the root was anchored")
	}
}

func TestEveryFieldIsBoundIntoTheLeaf(t *testing.T) {
	addrA, _ := ParseAddress(wallet(1))
	addrB, _ := ParseAddress(wallet(2))
	base := EntitlementLeaf(0, addrA, anchor(1), 5, false)

	variants := map[string]merkle.Hash{
		"leafIndex": EntitlementLeaf(1, addrA, anchor(1), 5, false),
		"holder":    EntitlementLeaf(0, addrB, anchor(1), 5, false),
		"anchor":    EntitlementLeaf(0, addrA, anchor(2), 5, false),
		"units":     EntitlementLeaf(0, addrA, anchor(1), 6, false),
		"excluded":  EntitlementLeaf(0, addrA, anchor(1), 5, true),
	}
	for name, v := range variants {
		if v == base {
			t.Errorf("%s is not bound into the leaf", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Addresses
// ---------------------------------------------------------------------------

func TestParseAddressRejectsNonCanonicalForms(t *testing.T) {
	bad := map[string]string{
		"uppercase hex":  "0x000000000000000000000000000000000000DEAD",
		"mixed case":     "0x000000000000000000000000000000000000DeAd",
		"no prefix":      "000000000000000000000000000000000000dead",
		"too short":      "0x0000000000000000000000000000000000dead",
		"too long":       "0x000000000000000000000000000000000000deadff",
		"empty":          "",
		"non hex":        "0x00000000000000000000000000000000000ghijk",
		"wrong prefix":   "0X000000000000000000000000000000000000dead",
	}
	for name, addr := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseAddress(addr); !errors.Is(err, ErrBadAddress) {
				t.Fatalf("want ErrBadAddress for %q, got %v", addr, err)
			}
		})
	}
}

// TestAddressCaseWouldChangeTheLeaf is why uppercase is rejected rather than folded.
func TestAddressCaseWouldChangeTheLeaf(t *testing.T) {
	// A checksummed address is the same address. If it parsed, it would still produce the same bytes,
	// so the danger is not a differing leaf but a differing *document*: the allowlist rejects mixed
	// case, so a snapshot carrying one would fail validation at pin time, midway through a period.
	if _, err := ParseAddress("0x000000000000000000000000000000000000DeAd"); err == nil {
		t.Fatal("mixed case must be refused at the boundary rather than at pin time")
	}
}

func TestAddressRoundTrip(t *testing.T) {
	in := "0x000000000000000000000000000000000000dead"
	a, err := ParseAddress(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatAddress(a); got != in {
		t.Errorf("round trip gave %q, want %q", got, in)
	}
}

// ---------------------------------------------------------------------------
// Determinism
// ---------------------------------------------------------------------------

// TestRootIsIndependentOfInputOrder is what makes the anchored root reproducible.
//
// Leaf index is part of the leaf, so the root depends on ordering. Whatever order the database
// returned is not a function of the data: a query plan change, a new index, or a reconciliation that
// rewrote rows would reorder the register and produce a different root for identical holdings.
func TestRootIsIndependentOfInputOrder(t *testing.T) {
	holdings := liveRegister()

	forward, err := Build(baseInput(holdings))
	if err != nil {
		t.Fatal(err)
	}

	reversed := make([]Holding, len(holdings))
	for i, h := range holdings {
		reversed[len(holdings)-1-i] = h
	}
	backward, err := Build(baseInput(reversed))
	if err != nil {
		t.Fatal(err)
	}

	if forward.MerkleRoot != backward.MerkleRoot {
		t.Fatalf("reversing the register changed the root:\n %s\n %s",
			forward.MerkleRoot.Hex(), backward.MerkleRoot.Hex())
	}

	// A deterministic shuffle, to catch an ordering that happens to be symmetric under reversal.
	shuffled := make([]Holding, len(holdings))
	copy(shuffled, holdings)
	for i := range shuffled {
		j := (i*7 + 3) % len(shuffled)
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	third, err := Build(baseInput(shuffled))
	if err != nil {
		t.Fatal(err)
	}
	if third.MerkleRoot != forward.MerkleRoot {
		t.Fatal("shuffling the register changed the root")
	}
}

func TestBuildDoesNotMutateCallerInput(t *testing.T) {
	holdings := liveRegister()
	before := make([]string, len(holdings))
	for i, h := range holdings {
		before[i] = h.InvestorID
	}

	if _, err := Build(baseInput(holdings)); err != nil {
		t.Fatal(err)
	}
	for i, h := range holdings {
		if h.InvestorID != before[i] {
			t.Fatalf("Build reordered the caller's slice at index %d", i)
		}
	}
}

func TestLeafIndicesAreContiguousFromZero(t *testing.T) {
	snap, err := Build(baseInput(liveRegister()))
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range snap.Lines {
		if l.LeafIndex != uint32(i) {
			t.Fatalf("line %d carries leaf index %d; the database requires unique contiguous "+
				"indices and a gap would make a published proof unverifiable", i, l.LeafIndex)
		}
	}
}

// ---------------------------------------------------------------------------
// Rejections
// ---------------------------------------------------------------------------

func TestDuplicatesRejected(t *testing.T) {
	t.Run("anchor", func(t *testing.T) {
		h := []Holding{holding(1, 5, false), holding(2, 3, false)}
		h[1].InvestorAnchor = h[0].InvestorAnchor
		if _, err := Build(baseInput(h)); !errors.Is(err, ErrDuplicateAnchor) {
			t.Fatalf("want ErrDuplicateAnchor, got %v", err)
		}
	})

	t.Run("wallet", func(t *testing.T) {
		h := []Holding{holding(1, 5, false), holding(2, 3, false)}
		h[1].WalletAddress = h[0].WalletAddress
		if _, err := Build(baseInput(h)); !errors.Is(err, ErrDuplicateWallet) {
			t.Fatalf("want ErrDuplicateWallet, got %v", err)
		}
	})

	t.Run("investor", func(t *testing.T) {
		h := []Holding{holding(1, 5, false), holding(2, 3, false)}
		h[1].InvestorID = h[0].InvestorID
		if _, err := Build(baseInput(h)); !errors.Is(err, ErrDuplicateInvestor) {
			t.Fatalf("want ErrDuplicateInvestor, got %v", err)
		}
	})
}

func TestZeroUnitsRejected(t *testing.T) {
	h := []Holding{holding(1, 5, false), holding(2, 0, false)}
	if _, err := Build(baseInput(h)); !errors.Is(err, ErrZeroUnits) {
		t.Fatalf("want ErrZeroUnits, got %v", err)
	}
}

func TestZeroAnchorRejected(t *testing.T) {
	h := []Holding{holding(1, 5, false)}
	h[0].InvestorAnchor = [32]byte{}
	if _, err := Build(baseInput(h)); !errors.Is(err, ErrZeroAnchor) {
		t.Fatalf("want ErrZeroAnchor, got %v", err)
	}
}

func TestEmptyRegisterRejected(t *testing.T) {
	if _, err := Build(baseInput(nil)); !errors.Is(err, ErrNoHoldings) {
		t.Fatalf("want ErrNoHoldings, got %v", err)
	}
}

// TestExpectedTotalMismatchRejected covers a register that has drifted from the issued count.
//
// The register is the thing under test. Summing it and trusting the result would anchor a holding
// dropped by a bad join or double-counted by a retry, so the sum is compared against the
// independently known issued count.
func TestExpectedTotalMismatchRejected(t *testing.T) {
	in := baseInput(liveRegister())
	in.ExpectedTotalUnits = 500
	if _, err := Build(in); err != nil {
		t.Fatalf("the fixture sums to 500 and must pass: %v", err)
	}

	// Drop one holder's units and the sum no longer matches.
	holdings := liveRegister()
	holdings[5].Units = 1
	in2 := baseInput(holdings)
	in2.ExpectedTotalUnits = 500
	if _, err := Build(in2); !errors.Is(err, ErrUnitsMismatch) {
		t.Fatalf("want ErrUnitsMismatch, got %v", err)
	}
}

// TestRecordDateBeforePeriodEndRejected mirrors the database constraint.
//
// A record date before the period ends would determine entitlement from a register that had not yet
// seen the period's own transfers.
func TestRecordDateBeforePeriodEndRejected(t *testing.T) {
	in := baseInput(liveRegister())
	in.RecordDate = periodEnd.AddDate(0, 0, -1)
	if _, err := Build(in); !errors.Is(err, ErrRecordDateBefore) {
		t.Fatalf("want ErrRecordDateBefore, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Proofs
// ---------------------------------------------------------------------------

func TestEveryHolderCanProveTheirEntitlement(t *testing.T) {
	snap, err := Build(baseInput(liveRegister()))
	if err != nil {
		t.Fatal(err)
	}

	for _, line := range snap.Lines {
		proof, err := snap.Proof(line.LeafIndex)
		if err != nil {
			t.Fatalf("leaf %d: %v", line.LeafIndex, err)
		}
		if !snap.VerifyLine(line, proof) {
			t.Fatalf("holder at leaf %d cannot prove their own entitlement", line.LeafIndex)
		}
	}
}

// TestProofForWalletIsTheLookupAHolderActuallyPerforms covers the access path that matters.
//
// A holder knows their wallet address, not their leaf index. If locating a line required the index or
// the anchor, verification would depend on AcreSync telling them a value they cannot derive.
func TestProofForWalletIsTheLookupAHolderActuallyPerforms(t *testing.T) {
	snap, err := Build(baseInput(liveRegister()))
	if err != nil {
		t.Fatal(err)
	}

	line, proof, err := snap.ProofForWallet(wallet(500))
	if err != nil {
		t.Fatal(err)
	}
	if line.Units != 75 {
		t.Errorf("units = %d, want 75", line.Units)
	}
	if !snap.VerifyLine(line, proof) {
		t.Fatal("the proof returned for a wallet must verify")
	}

	if _, _, err := snap.ProofForWallet(wallet(9999)); err == nil {
		t.Error("a wallet not on the register must be reported, not silently proved")
	}
}

// TestVerifyLineRecomputesTheLeaf confirms the check is not circular.
//
// Comparing a stored leaf against a root built from those same stored leaves proves nothing. The
// verification has to recompute the leaf from the published fields, which is what an external verifier
// does.
func TestVerifyLineRecomputesTheLeaf(t *testing.T) {
	snap, err := Build(baseInput(liveRegister()))
	if err != nil {
		t.Fatal(err)
	}

	line := snap.Lines[3]
	proof, err := snap.Proof(line.LeafIndex)
	if err != nil {
		t.Fatal(err)
	}

	// A restated unit count must fail, even though the stored Leaf field still holds the original
	// digest.
	tampered := line
	tampered.Units = line.Units + 1
	if snap.VerifyLine(tampered, proof) {
		t.Fatal("a restated unit count verified; VerifyLine must recompute the leaf rather than " +
			"trusting the stored digest")
	}

	// Flipping the classification must also fail.
	reclassified := line
	reclassified.ExcludedFromHolderCount = !line.ExcludedFromHolderCount
	if snap.VerifyLine(reclassified, proof) {
		t.Fatal("a reclassified holder verified against the original proof")
	}
}

func TestProofOutOfRange(t *testing.T) {
	snap, err := Build(baseInput(liveRegister()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snap.Proof(uint32(len(snap.Lines))); err == nil {
		t.Error("an out-of-range leaf index must be reported")
	}
}

func TestUnitsFor(t *testing.T) {
	snap, err := Build(baseInput(liveRegister()))
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := snap.UnitsFor(wallet(500)); !ok || u != 75 {
		t.Errorf("UnitsFor = %d, %v; want 75, true", u, ok)
	}
	if _, ok := snap.UnitsFor(wallet(9999)); ok {
		t.Error("an absent wallet must report false")
	}
}

// ---------------------------------------------------------------------------
// The document
// ---------------------------------------------------------------------------

func TestDocumentPassesTheFieldAllowlist(t *testing.T) {
	snap, err := Build(baseInput(liveRegister()))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := snap.Document()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ipfsguard.ValidateAndCanonicalize(ipfsguard.DocSnapshot, raw); err != nil {
		t.Fatalf("the snapshot document must satisfy the allowlist: %v", err)
	}
}

// TestDocumentCarriesNoInternalIdentifier checks what is deliberately absent.
//
// The investor UUID is an internal identifier. Publishing it would let anyone who ever sees one
// correlate a holder across every period the scheme runs, and IPFS content cannot be withdrawn.
func TestDocumentCarriesNoInternalIdentifier(t *testing.T) {
	holdings := liveRegister()
	snap, err := Build(baseInput(holdings))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := snap.Document()
	if err != nil {
		t.Fatal(err)
	}

	for _, h := range holdings {
		if strings.Contains(string(raw), h.InvestorID) {
			t.Fatalf("investor id %s reached the document", h.InvestorID)
		}
	}
	if strings.Contains(string(raw), "investorId") {
		t.Error("the investorId key must not appear")
	}
}

// TestVerifierJourneyFromDocumentAlone is the claim the anchored root makes, executed end to end.
//
// Nothing here touches the Snapshot object's internals. The document is parsed as a third party would
// parse it, each leaf is recomputed from the published fields under the published rule, the tree is
// rebuilt, and the root is compared against the anchored value. If this passes, an investor with the
// pinned document and a block explorer can confirm their entitlement with no access to our systems.
func TestVerifierJourneyFromDocumentAlone(t *testing.T) {
	snap, err := Build(baseInput(liveRegister()))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := snap.Document()
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		TotalUnits      uint32 `json:"totalUnits"`
		DistinctHolders uint32 `json:"distinctHolders"`
		MerkleRoot      string `json:"merkleRoot"`
		AlgoVersion     int    `json:"algoVersion"`
		Lines           []struct {
			LeafIndex      uint32 `json:"leafIndex"`
			Holder         string `json:"holder"`
			Units          uint32 `json:"units"`
			Excluded       bool   `json:"excluded"`
			InvestorAnchor string `json:"investorAnchor"`
		} `json:"lines"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	if doc.AlgoVersion != AlgoVersion {
		t.Fatalf("algoVersion = %d; a verifier needs it to know which leaf rule applies", doc.AlgoVersion)
	}

	// Rebuild every leaf from published fields alone.
	leaves := make([]merkle.Hash, 0, len(doc.Lines))
	var sumUnits, countable uint32
	for _, l := range doc.Lines {
		addr, err := ParseAddress(l.Holder)
		if err != nil {
			t.Fatalf("leaf %d: %v", l.LeafIndex, err)
		}
		a, err := merkle.ParseHash(l.InvestorAnchor)
		if err != nil {
			t.Fatalf("leaf %d: %v", l.LeafIndex, err)
		}

		leaves = append(leaves, EntitlementLeaf(l.LeafIndex, addr, a, l.Units, l.Excluded))
		sumUnits += l.Units
		if !l.Excluded {
			countable++
		}
	}

	root, err := merkle.RootOf(leaves)
	if err != nil {
		t.Fatal(err)
	}
	if root.Hex() != doc.MerkleRoot {
		t.Fatalf("root rebuilt from the document is %s, document states %s",
			root.Hex(), doc.MerkleRoot)
	}
	if root != snap.MerkleRoot {
		t.Fatal("the rebuilt root differs from the one that would be anchored")
	}

	// The published totals must be recomputable from the published lines, or a verifier has to take
	// them on faith.
	if sumUnits != doc.TotalUnits {
		t.Errorf("lines sum to %d units, header states %d", sumUnits, doc.TotalUnits)
	}
	if countable != doc.DistinctHolders {
		t.Errorf("%d countable lines, header states %d", countable, doc.DistinctHolders)
	}
}

func TestDocumentTimestampIsUTCWithZSuffix(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	in := baseInput(liveRegister())
	in.TakenAt = time.Date(2026, 9, 30, 18, 30, 0, 0, ist)

	snap, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := snap.Document()
	if err != nil {
		t.Fatal(err)
	}

	// A numeric offset is a second spelling of one instant, and two spellings would give one snapshot
	// two encodings and therefore two digests.
	if strings.Contains(string(raw), "+05:30") {
		t.Fatal("a numeric timezone offset reached the document")
	}
	if !strings.Contains(string(raw), "2026-09-30T13:00:00Z") {
		t.Errorf("expected the UTC instant with a Z suffix, got: %s", extract(string(raw), "takenAt"))
	}
}

func extract(doc, key string) string {
	i := strings.Index(doc, `"`+key+`"`)
	if i < 0 {
		return "<absent>"
	}
	end := i + 40
	if end > len(doc) {
		end = len(doc)
	}
	return doc[i:end]
}

func TestDocumentHexIsLowercase(t *testing.T) {
	snap, err := Build(baseInput(liveRegister()))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := snap.Document()
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{"0xA", "0xB", "0xC", "0xD", "0xE", "0xF"} {
		if strings.Contains(string(raw), frag) {
			t.Errorf("uppercase hex %q in the document", frag)
		}
	}
}

// TestSingleHolderRegisterWorks covers the degenerate tree, where the root is the leaf itself.
func TestSingleHolderRegisterWorks(t *testing.T) {
	snap, err := Build(baseInput([]Holding{holding(1, 500, false)}))
	if err != nil {
		t.Fatal(err)
	}
	if snap.MerkleRoot != snap.Lines[0].Leaf {
		t.Error("a one-leaf tree must have the leaf as its root")
	}
	proof, err := snap.Proof(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(proof) != 0 {
		t.Errorf("a one-leaf proof should be empty, got %d elements", len(proof))
	}
	if !snap.VerifyLine(snap.Lines[0], proof) {
		t.Error("the sole holder must be able to prove their entitlement")
	}
}
