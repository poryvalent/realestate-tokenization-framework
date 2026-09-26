package bidbook

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/acresync/orchestrator/internal/ipfsguard"
	"github.com/acresync/orchestrator/internal/merkle"
)

// SchemaVersion is the document schema generation.
const SchemaVersion = 1

// Document renders the frozen book as JSON for publishing.
//
// # Why the whole book is published, including the losers
//
// This document is pinned and its digest anchored before the seed is committed, which is what stops the
// book being edited once the draw is known. For that to mean anything the document has to contain every
// bid, not just the ones that later win. A reader given only the winners cannot tell whether a losing
// bid was in the book at all, so the people with the strongest reason to check the draw would be the
// only ones unable to.
//
// # What is published and what is not
//
// Each line carries the bid reference, the investor anchor, the units and the price. It does not carry
// the investor identifier, a name, a bank account, or the funds-block reference. The reference is the
// investor's own receipt and the anchor is an HMAC under a KMS-held key, so a line identifies a bid
// without identifying a person.
//
// The output is ordinary JSON. Canonicalisation belongs to the publisher, which runs the field allowlist
// over it and returns the canonical bytes it validated, so there is one canonicaliser in the pipeline and
// one set of bytes that was checked for personal data.
func (b *Book) Document() ([]byte, error) {
	if !isCanonicalUUID(b.OfferID) {
		// The allowlist would reject it later. Failing here names the field, which a pattern-match
		// failure inside the validator does not.
		return nil, fmt.Errorf("bidbook: offerId %q is not a lowercase canonical UUID", b.OfferID)
	}

	leaves := make([]map[string]any, 0, len(b.Lines))
	for _, l := range b.Lines {
		if l.PricePerUnitPaise < 0 {
			return nil, fmt.Errorf("bidbook: bid %s has a negative price", l.BidRef)
		}

		leaves = append(leaves, map[string]any{
			// Position in the tree. Part of the leaf preimage, so a verifier needs it to recompute.
			"leafIndex": l.LeafIndex,

			// The investor's receipt, and the key the book is ordered by.
			"bidRef": l.BidRef,

			"investorAnchor": hexBytes32(l.InvestorAnchor),

			"unitsBid": l.UnitsBid,

			// Published as paise, an integer. A rupee figure would need a decimal point, and a decimal
			// point in a document whose digest is anchored is a second spelling waiting to happen.
			"pricePerUnitPaise": uint64(l.PricePerUnitPaise),
		})

		// BidID and InvestorID are deliberately absent. Both are internal identifiers, and the internal
		// identifier is what links a bid to the KYC record behind it.
	}

	doc := map[string]any{
		"docType":       string(ipfsguard.DocBidbook),
		"schemaVersion": SchemaVersion,
		"schemeRef":     hexBytes32(b.SchemeRef),

		"offerId":  b.OfferID,
		"frozenAt": isoDateTime(b.FrozenAt),

		"merkleRoot": b.MerkleRoot.Hex(),

		"leafCount":       b.LeafCount,
		"totalUnitsBid":   b.TotalUnitsBid,
		"distinctBidders": b.DistinctBidders,

		// Which leaf encoding and allocation rules apply. Without it a future v2 would make every
		// existing document ambiguous rather than merely old.
		"algoVersion": b.AlgoVersion,

		"leaves": leaves,
	}

	// TotalAmountPaise is deliberately not published. It is derivable by summing the lines, and a stated
	// total that disagreed with the lines would give a reader two answers and no rule for choosing.

	return json.Marshal(doc)
}

// isoDateTime renders an instant in the single form the allowlist accepts.
//
// Always UTC with a Z suffix. A numeric offset such as +05:30 is a second spelling of the same instant,
// and two spellings would give one book two documents and therefore two digests to anchor.
func isoDateTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// hexBytes32 renders a digest as 0x-prefixed lowercase hex, the only form the allowlist accepts.
func hexBytes32(h merkle.Hash) string {
	return "0x" + strings.ToLower(hex.EncodeToString(h[:]))
}

// isCanonicalUUID reports whether s is a lowercase canonical UUID.
func isCanonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isDigit := c >= '0' && c <= '9'
			isLowerHex := c >= 'a' && c <= 'f'
			if !isDigit && !isLowerHex {
				return false
			}
		}
	}
	return true
}
