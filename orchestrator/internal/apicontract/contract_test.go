// Package apicontract holds the drift guard between the published API contract and the Go domain.
//
// # Why this test exists
//
// docs/api/openapi.yaml is what the frontend is built against, and it was written by reading the domain
// packages. That makes it correct the day it was written and says nothing about tomorrow. Add a value to
// offer.Status and the contract silently describes a system that no longer exists; the frontend's
// exhaustive switch then falls through on a status the backend really sends, in production, on a state
// the UI has no branch for.
//
// A comment claiming the two agree is exactly the kind of thing that stops being true without anyone
// noticing. So the agreement is asserted instead.
//
// The spec is parsed as YAML rather than pattern-matched. A hand-rolled extractor would agree with its
// own misreading of the document forever, which is the same trap the CID encoder tests avoid by
// cross-checking against a real decoder.
package apicontract

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/acresync/orchestrator/internal/asba"
	"github.com/acresync/orchestrator/internal/ballot"
	"github.com/acresync/orchestrator/internal/ballotrun"
	"github.com/acresync/orchestrator/internal/entitlement"
	"github.com/acresync/orchestrator/internal/ndcf"
	"github.com/acresync/orchestrator/internal/offer"
	"github.com/acresync/orchestrator/internal/payout"
	"github.com/acresync/orchestrator/internal/period"
)

// specPath is the published contract, relative to this package.
func specPath() string {
	return filepath.Join("..", "..", "..", "docs", "api", "openapi.yaml")
}

// spec is the subset of OpenAPI this test needs.
type spec struct {
	Components struct {
		// Only the enum is read. `type` is deliberately absent: several schemas declare it as a list,
		// such as `type: [string, 'null']`, so a string field here would fail the whole parse and every
		// comparison below would report a YAML error instead of a drift.
		Schemas map[string]struct {
			Enum []string `yaml:"enum"`
		} `yaml:"schemas"`
	} `yaml:"components"`
	Paths map[string]map[string]struct {
		OperationID string   `yaml:"operationId"`
		Tags        []string `yaml:"tags"`
	} `yaml:"paths"`
}

func loadSpec(t *testing.T) spec {
	t.Helper()

	raw, err := os.ReadFile(specPath())
	if err != nil {
		t.Fatalf("reading the API contract: %v", err)
	}

	var s spec
	if err := yaml.Unmarshal(raw, &s); err != nil {
		t.Fatalf("the API contract is not parseable YAML: %v", err)
	}
	if len(s.Components.Schemas) == 0 {
		t.Fatal("the contract declares no schemas; the parse shape is probably wrong rather than the file")
	}
	return s
}

// enumOf returns a schema's enum, failing if the schema is absent or is not an enum.
//
// Failing on absence matters. If a schema were renamed, a lenient lookup would return an empty list and
// the comparison below would pass trivially against a contract that no longer describes anything.
func enumOf(t *testing.T, s spec, schema string) []string {
	t.Helper()

	got, ok := s.Components.Schemas[schema]
	if !ok {
		t.Fatalf("the contract has no schema %q; if it was renamed this test must follow it rather than "+
			"be deleted", schema)
	}
	if len(got.Enum) == 0 {
		t.Fatalf("schema %q declares no enum values", schema)
	}
	return got.Enum
}

// assertSameSet compares a contract enum against the domain, ignoring order.
func assertSameSet(t *testing.T, schema string, contract, domain []string) {
	t.Helper()

	inContract := make(map[string]bool, len(contract))
	for _, v := range contract {
		if inContract[v] {
			t.Errorf("%s: the contract lists %q twice", schema, v)
		}
		inContract[v] = true
	}

	inDomain := make(map[string]bool, len(domain))
	for _, v := range domain {
		inDomain[v] = true
	}

	for _, v := range domain {
		if !inContract[v] {
			t.Errorf("%s: the domain has %q and the contract does not. A frontend switching on this "+
				"enum will fall through on a value the backend really sends", schema, v)
		}
	}
	for _, v := range contract {
		if !inDomain[v] {
			t.Errorf("%s: the contract offers %q and the domain does not. The frontend may have a "+
				"branch for a state that can never occur", schema, v)
		}
	}
}

// assertSameOrder compares a contract enum against the domain including position.
func assertSameOrder(t *testing.T, schema string, contract, domain []string, why string) {
	t.Helper()

	assertSameSet(t, schema, contract, domain)

	if len(contract) != len(domain) {
		return // already reported by the set comparison
	}
	for i := range domain {
		if contract[i] != domain[i] {
			t.Fatalf("%s: position %d is %q in the contract and %q in the domain. Order matters here: %s",
				schema, i, contract[i], domain[i], why)
		}
	}
}

func strs[T ~string](in []T) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, string(v))
	}
	return out
}

// --- the enums the frontend switches on -----------------------------------------------------------

func TestOfferStatusMatches(t *testing.T) {
	s := loadSpec(t)
	assertSameSet(t, "OfferStatus", enumOf(t, s, "OfferStatus"), strs(offer.AllStatuses()))
}

func TestPeriodStatusMatches(t *testing.T) {
	s := loadSpec(t)
	assertSameSet(t, "PeriodStatus", enumOf(t, s, "PeriodStatus"), strs(period.AllStatuses()))
}

func TestBallotStageMatches(t *testing.T) {
	s := loadSpec(t)
	assertSameSet(t, "BallotStage", enumOf(t, s, "BallotStage"), strs(ballotrun.AllStages()))
}

func TestAsbaBlockStatusMatches(t *testing.T) {
	s := loadSpec(t)
	assertSameSet(t, "AsbaBlockStatus", enumOf(t, s, "AsbaBlockStatus"), strs(asba.AllStatuses()))
}

func TestInvestorClassMatches(t *testing.T) {
	s := loadSpec(t)
	assertSameSet(t, "InvestorClass", enumOf(t, s, "InvestorClass"),
		strs(entitlement.AllInvestorClasses()))
}

func TestNdcfLineTypeMatches(t *testing.T) {
	s := loadSpec(t)
	assertSameSet(t, "NdcfLineType", enumOf(t, s, "NdcfLineType"), strs(ndcf.AllLineTypes()))
}

func TestNdcfDirectionMatches(t *testing.T) {
	s := loadSpec(t)
	assertSameSet(t, "NdcfDirection", enumOf(t, s, "NdcfDirection"), strs(ndcf.AllDirections()))
}

func TestReconciliationStatusMatches(t *testing.T) {
	s := loadSpec(t)
	assertSameSet(t, "ReconciliationStatus", enumOf(t, s, "ReconciliationStatus"),
		strs(period.AllReconciliationStatuses()))
}

// TestAllocationOutcomeMatchesInOrder is the one where position is load-bearing.
//
// The numeric position of an outcome is the code written into the allotment leaf preimage, and the same
// ordering appears in the Solidity enum and the allocation_outcome Postgres enum. Reordering the contract
// would not merely mislabel a field, it would describe a different hash.
func TestAllocationOutcomeMatchesInOrder(t *testing.T) {
	s := loadSpec(t)
	assertSameOrder(t, "AllocationOutcome",
		enumOf(t, s, "AllocationOutcome"), strs(ballot.AllOutcomes()),
		"the numeric position is part of the allotment leaf preimage and must match the Solidity enum")
}

// --- the payout vocabulary trap -------------------------------------------------------------------

// TestPayoutStatusIsOursNotTheProviders guards a distinction that is easy to lose.
//
// There are two payout vocabularies. payout.DBStatus is AcreSync's own, uppercase, and is what the
// contract publishes. payout.Status is the payment provider's, lowercase, and must never reach the API,
// because a frontend rendering "processed" would be coupled to RazorpayX's words and would start lying
// the moment the provider changed.
func TestPayoutStatusIsOursNotTheProviders(t *testing.T) {
	s := loadSpec(t)
	contract := enumOf(t, s, "PayoutStatus")

	assertSameSet(t, "PayoutStatus", contract, strs(payout.AllDBStatuses()))

	provider := make(map[string]bool)
	for _, v := range payout.AllStatuses() {
		provider[string(v)] = true
	}
	for _, v := range contract {
		if provider[v] {
			t.Errorf("PayoutStatus: %q is the payment provider's vocabulary, not ours. Publishing it "+
				"couples the frontend to a vendor's words", v)
		}
	}
}

// --- structural expectations ----------------------------------------------------------------------

// TestPublicEndpointsRequireNoAuth protects the transparency surface.
//
// The public endpoints are the reason the system is credible: a third party must be able to check an
// allotment or an entitlement without any access to AcreSync. If one of them quietly acquired an auth
// requirement, verification would become something only we could perform, and the claim would be hollow
// while every test still passed.
func TestPublicEndpointsRequireNoAuth(t *testing.T) {
	raw, err := os.ReadFile(specPath())
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string   `yaml:"operationId"`
			Tags        []string `yaml:"tags"`
			Security    *[]any   `yaml:"security"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	var checked int
	for path, methods := range doc.Paths {
		for method, op := range methods {
			isPublic := false
			for _, tag := range op.Tags {
				if tag == "public" {
					isPublic = true
				}
			}
			if !isPublic {
				continue
			}
			checked++

			// An empty security array is what switches off the document-level default.
			if op.Security == nil {
				t.Errorf("%s %s (%s) is tagged public but does not clear the default security "+
					"requirement, so it would demand a token", method, path, op.OperationID)
				continue
			}
			if len(*op.Security) != 0 {
				t.Errorf("%s %s (%s) is tagged public but declares a security requirement",
					method, path, op.OperationID)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no public operations found; the tag or the parse shape has changed and this test " +
			"would silently protect nothing")
	}
	t.Logf("verified %d public operations require no authentication", checked)
}

// TestMutationsRequireAnIdempotencyKey is why a retry cannot double-pay.
//
// Every POST reaches something with a side effect that must not happen twice: a bank block on an
// investor's account, or a transaction anchored on-chain. The key is the only thing making a retry after
// a dropped connection safe, so it is required rather than optional.
func TestMutationsRequireAnIdempotencyKey(t *testing.T) {
	raw, err := os.ReadFile(specPath())
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `yaml:"operationId"`
			Parameters  []struct {
				Ref  string `yaml:"$ref"`
				Name string `yaml:"name"`
			} `yaml:"parameters"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	var checked int
	for path, methods := range doc.Paths {
		op, ok := methods["post"]
		if !ok {
			continue
		}
		checked++

		found := false
		for _, p := range op.Parameters {
			if p.Name == "Idempotency-Key" ||
				p.Ref == "#/components/parameters/IdempotencyKey" {
				found = true
			}
		}
		if !found {
			t.Errorf("POST %s (%s) does not require an Idempotency-Key. A retry after a dropped "+
				"connection could place a second funds block or anchor a second transaction",
				path, op.OperationID)
		}
	}

	if checked == 0 {
		t.Fatal("no POST operations found; the parse shape has changed")
	}
	t.Logf("verified %d mutations require an idempotency key", checked)
}

// TestEveryDomainEnumIsCovered stops this file rotting by omission.
//
// The tests above each pin one enum. Nothing stops a future enum being added to the domain and exposed in
// the contract with no test at all, which would be a silent gap in exactly the place this package exists
// to protect. This asserts the count of enum schemas the contract publishes is one we have accounted for.
func TestEveryDomainEnumIsCovered(t *testing.T) {
	s := loadSpec(t)

	// Enums that mirror a Go domain type and are pinned by a test above.
	pinned := map[string]bool{
		"OfferStatus":          true,
		"PeriodStatus":         true,
		"BallotStage":          true,
		"AsbaBlockStatus":      true,
		"InvestorClass":        true,
		"NdcfLineType":         true,
		"NdcfDirection":        true,
		"ReconciliationStatus": true,
		"AllocationOutcome":    true,
		"PayoutStatus":         true,
	}

	// Enums that are API-only vocabulary with no single Go counterpart. Listed explicitly so adding a
	// new enum forces a decision rather than defaulting to unchecked.
	//
	// This list earned its keep immediately: adding DocumentPurpose to the contract failed this test
	// until it was accounted for here, which is the behaviour intended.
	apiOnly := map[string]bool{
		"SettlementStage": true, // mirrors a Solidity enum, not a Go string type
		"OutboxStatus":    true,

		// Upload intents are a transport concern. The purpose decides where a document is filed and
		// whether its digest is anchored, but no Go type enumerates it yet; when the handlers add one,
		// move this entry up into `pinned`.
		"DocumentPurpose": true,
	}

	var unaccounted []string
	for name, schema := range s.Components.Schemas {
		if len(schema.Enum) == 0 {
			continue
		}
		if pinned[name] || apiOnly[name] {
			continue
		}
		unaccounted = append(unaccounted, name)
	}

	for _, name := range unaccounted {
		t.Errorf("schema %q is an enum with no drift test. Either pin it against its Go type or add it "+
			"to apiOnly with a reason", name)
	}
}
