package snapshot

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/acresync/orchestrator/internal/ipfsguard"
)

// SchemaVersion is the document schema generation.
const SchemaVersion = 1

// Document renders the snapshot as JSON for publishing.
//
// # What a verifier can do with this
//
// Everything needed to reproduce the anchored root, and nothing that identifies anybody. A holder
// finds their line by wallet address, recomputes their leaf under the rule in encoding.go, rebuilds
// the tree from the published lines, and compares the root against the one in the period struct
// on-chain. No access to AcreSync systems is involved at any step.
//
// The output is ordinary JSON. Canonicalisation belongs to the publisher, which runs the field
// allowlist over it and returns the canonical bytes it validated, so there is exactly one
// canonicaliser in the pipeline and exactly one set of bytes that was checked for personal data.
func (s *Snapshot) Document() ([]byte, error) {
	lines := make([]map[string]any, 0, len(s.Lines))
	for _, l := range s.Lines {
		lines = append(lines, map[string]any{
			"leafIndex": l.LeafIndex,

			// The wallet address, which the holder already knows and which is public on-chain anyway.
			"holder": l.WalletAddress,

			"units": l.Units,

			// Published under the name the allowlist expects. Excluded from the holder count, not from
			// the distribution; the denominator is totalUnits.
			"excluded": l.ExcludedFromHolderCount,

			// The anchor, not a name, not a PAN, not an email. An HMAC under a KMS-held key, so it is
			// stable across periods and not reversible by enumerating a small identifier space.
			"investorAnchor": hexBytes32(l.InvestorAnchor),
		})

		// InvestorID is deliberately absent. It is an internal identifier, and publishing it would
		// let anyone who ever sees one correlate a holder across every period the scheme runs.
	}

	doc := map[string]any{
		"docType":       string(ipfsguard.DocSnapshot),
		"schemaVersion": SchemaVersion,
		"schemeRef":     hexBytes32(s.SchemeRef),

		"periodId":   s.PeriodID,
		"recordDate": isoDate(s.RecordDate),
		"takenAt":    isoDateTime(s.TakenAt),

		"totalUnits":      s.TotalUnits,
		"distinctHolders": s.DistinctHolders,

		"merkleRoot": s.MerkleRoot.Hex(),

		// The encoding version, so a verifier knows which leaf rule to apply. Without it, a future v2
		// encoding would make old documents ambiguous rather than merely old.
		"algoVersion": s.AlgoVersion,

		"lines": lines,
	}

	return json.Marshal(doc)
}

// isoDateTime renders an instant in the single form the allowlist accepts.
//
// Always UTC with a Z suffix. A numeric offset such as +05:30 is a second spelling of the same
// instant, and two spellings would give one snapshot two document encodings and therefore two
// digests to anchor.
func isoDateTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// hexBytes32 renders a digest as 0x-prefixed lowercase hex, the only form the allowlist accepts.
func hexBytes32(b [32]byte) string {
	return "0x" + strings.ToLower(hex.EncodeToString(b[:]))
}
