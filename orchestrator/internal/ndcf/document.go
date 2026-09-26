package ndcf

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/acresync/orchestrator/internal/ipfsguard"
)

// SchemaVersion is the document schema generation.
const SchemaVersion = 1

// StatementInput is everything needed to render the statement for a period.
type StatementInput struct {
	SchemeRef [32]byte
	PeriodID  uint32

	// PeriodStart and PeriodEnd bound the period. Dates, not timestamps: a distribution period is a
	// calendar range, and attaching a time of day to it would invite a timezone to change which
	// period a receipt falls into.
	PeriodStart time.Time
	PeriodEnd   time.Time

	Plan  Plan
	Items []LineItem
}

// BuildStatement renders the NDCF statement as JSON for publishing.
//
// The output is ordinary JSON, not canonical form. Canonicalisation belongs to the publisher, which
// runs the IPFS field allowlist over it and returns the canonical bytes it validated. Two
// canonicalisers in one pipeline is one too many: the digest would depend on which path a document
// took, and only one of those paths is the one that was checked for personal data.
func BuildStatement(in StatementInput) ([]byte, error) {
	if in.PeriodEnd.Before(in.PeriodStart) {
		return nil, fmt.Errorf("ndcf: period end %s precedes start %s",
			isoDate(in.PeriodEnd), isoDate(in.PeriodStart))
	}
	if in.Plan.NDCFPaise <= 0 {
		return nil, fmt.Errorf("%w: statement requires a computed plan", ErrNotDistributable)
	}
	if len(in.Items) == 0 {
		return nil, ErrNoLineItems
	}

	lines, err := buildLineItems(in.Items)
	if err != nil {
		return nil, err
	}

	doc := map[string]any{
		"docType":       string(ipfsguard.DocNDCFStatement),
		"schemaVersion": SchemaVersion,
		"schemeRef":     hexBytes32(in.SchemeRef),
		"periodId":      in.PeriodID,
		"periodStart":   isoDate(in.PeriodStart),
		"periodEnd":     isoDate(in.PeriodEnd),
		"currency":      "INR",

		"ndcfPaise":        in.Plan.NDCFPaise,
		"distributedPaise": in.Plan.DistributedPaise,
		"distributionBps":  in.Plan.DistributionBps,

		"lineItems": lines,
	}

	return json.Marshal(doc)
}

func buildLineItems(items []LineItem) ([]map[string]any, error) {
	// A defensive copy, because sorting the caller's slice would reorder their records as a side
	// effect of rendering a document.
	sorted := make([]LineItem, len(items))
	copy(sorted, items)

	order := map[LineType]int{}
	for i, lt := range AllLineTypes() {
		order[lt] = i
	}

	// Sorted into a total order rather than kept in input order.
	//
	// The statement digest is anchored on-chain, so the byte layout has to be a function of the
	// content alone. Input order is not: the same period rebuilt from the database after a
	// reconciliation, or assembled by a second service, would order rows differently and produce a
	// different digest for identical facts. Every tiebreak below is included so the comparison is
	// total and no pair of distinct lines is left ambiguous.
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if order[a.LineType] != order[b.LineType] {
			return order[a.LineType] < order[b.LineType]
		}
		if a.AmountPaise != b.AmountPaise {
			return a.AmountPaise < b.AmountPaise
		}
		if a.SPVRef != b.SPVRef {
			return a.SPVRef < b.SPVRef
		}
		if a.PropertyRef != b.PropertyRef {
			return a.PropertyRef < b.PropertyRef
		}
		return hex.EncodeToString(a.EvidenceSHA256[:]) < hex.EncodeToString(b.EvidenceSHA256[:])
	})

	out := make([]map[string]any, 0, len(sorted))
	for i, it := range sorted {
		dir, err := it.LineType.Direction()
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i, err)
		}
		if it.AmountPaise <= 0 {
			return nil, fmt.Errorf("%w: line %d (%s) is %d",
				ErrNonPositiveAmount, i, it.LineType, it.AmountPaise)
		}
		if it.EvidenceSHA256 == ([32]byte{}) {
			return nil, fmt.Errorf("%w: line %d (%s)", ErrMissingEvidence, i, it.LineType)
		}

		// Direction is emitted even though it is derivable from the line type.
		//
		// Redundant by design. A third party verifying the statement should not have to know our
		// type-to-direction mapping to check that the arithmetic adds up, and having both in the
		// document means the mapping itself is auditable rather than implicit.
		line := map[string]any{
			"lineType":     string(it.LineType),
			"direction":    string(dir),
			"amountPaise":  it.AmountPaise,
			"evidenceHash": hexBytes32(it.EvidenceSHA256),
		}

		// Optional references are omitted when absent rather than emitted empty. The allowlist types
		// them as UUIDs, so an empty string would fail validation, and a scheme-level cost such as the
		// trustee fee genuinely belongs to no SPV or property.
		if it.SPVRef != "" {
			line["spvRef"] = it.SPVRef
		}
		if it.PropertyRef != "" {
			line["propertyRef"] = it.PropertyRef
		}

		// Description is deliberately absent.
		//
		// It is the only field on a line item that could carry a tenant name or other personal
		// detail, and IPFS content cannot be withdrawn. Omitting it here is the first of two
		// defences; the field allowlist rejects unrecognised keys independently, so both would have
		// to fail for it to leak.

		out = append(out, line)
	}
	return out, nil
}

func isoDate(t time.Time) string { return t.UTC().Format("2006-01-02") }

// hexBytes32 renders a digest as 0x-prefixed lowercase hex.
//
// Lowercase because the field allowlist rejects uppercase, so that one value has exactly one
// spelling. Two spellings of one digest would give two document encodings and therefore two anchors
// for the same content.
func hexBytes32(b [32]byte) string {
	return "0x" + strings.ToLower(hex.EncodeToString(b[:]))
}
