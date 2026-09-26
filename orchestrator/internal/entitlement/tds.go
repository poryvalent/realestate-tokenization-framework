// Package entitlement turns a distribution plan and a frozen register into per-holder amounts and
// payout instructions.
//
// # Where the boundary sits
//
// The gross entitlement is public: it is derivable by anyone from the distributed amount, the unit
// count and the snapshot, and it is what the on-chain entitlement anchors commit to. Tax is not. TDS
// rates differ by investor class, so publishing a net amount alongside a public unit count would
// disclose the holder's tax classification to anyone who divides one by the other.
//
// So this package computes both and keeps them apart: gross goes on-chain and into the pinned
// snapshot, tax and net stay in Postgres. The database schema says the same thing in a comment on
// tax_deductions, and the field allowlist has no key that would let a net amount reach IPFS.
package entitlement

import (
	"errors"
	"fmt"

	"github.com/acresync/orchestrator/internal/money"
)

// InvestorClass mirrors the investor_class Postgres enum.
//
// Country code is not a discriminator here: in an SM-REIT every holder is Indian, and the useful
// distinction is tax class.
type InvestorClass string

const (
	ClassResidentIndividual InvestorClass = "RESIDENT_IND"
	ClassNRI                InvestorClass = "NRI"
	ClassBodyCorporate      InvestorClass = "BODY_CORPORATE"
	ClassHUF                InvestorClass = "HUF"
)

// AllInvestorClasses lists every class.
func AllInvestorClasses() []InvestorClass {
	return []InvestorClass{ClassResidentIndividual, ClassNRI, ClassBodyCorporate, ClassHUF}
}

func (c InvestorClass) Valid() bool {
	switch c {
	case ClassResidentIndividual, ClassNRI, ClassBodyCorporate, ClassHUF:
		return true
	default:
		return false
	}
}

// IsResident reports whether the class is treated as resident for withholding.
func (c InvestorClass) IsResident() bool { return c != ClassNRI }

var (
	ErrNoTDSRule      = errors.New("entitlement: no withholding rule for investor class")
	ErrRateOutOfRange = errors.New("entitlement: withholding rate must be between 0 and 10000 bps")
)

// TDSRule is the withholding treatment for one investor class.
//
// # Why this is configuration rather than logic
//
// Rates and section numbers change with every finance act, and they have just changed shape: REIT
// distributions to residents were withheld under section 194LBA of the 1961 Act, and REIT
// disclosures for FY 2026-27 cite section 393(1), which is the Income Tax Act 2025 renumbering. A
// section reference compiled into Go would mean a code deployment to track a statutory renumbering,
// and worse, an old build would keep issuing certificates citing a repealed section.
//
// So the section is a string and the rate is a number, both supplied by configuration, and the Go
// code only does arithmetic.
type TDSRule struct {
	Class InvestorClass

	// Section is the statutory reference printed on the withholding certificate.
	Section string

	// RateBps is the withholding rate in basis points.
	RateBps uint16
}

func (r TDSRule) Validate() error {
	if !r.Class.Valid() {
		return fmt.Errorf("entitlement: unknown investor class %q", r.Class)
	}
	if r.Section == "" {
		return fmt.Errorf("entitlement: %s has no statutory section reference", r.Class)
	}
	if r.RateBps > 10_000 {
		return fmt.Errorf("%w: %s is %d", ErrRateOutOfRange, r.Class, r.RateBps)
	}
	return nil
}

// TDSTable maps investor classes to withholding rules.
type TDSTable struct {
	rules map[InvestorClass]TDSRule
}

// NewTDSTable builds a table, requiring a rule for every class.
//
// Total by construction. A missing class would surface as an unwithheld payment discovered by an
// auditor, so the table refuses to exist rather than defaulting a rate it was not told.
func NewTDSTable(rules ...TDSRule) (*TDSTable, error) {
	t := &TDSTable{rules: make(map[InvestorClass]TDSRule, len(rules))}
	for _, r := range rules {
		if err := r.Validate(); err != nil {
			return nil, err
		}
		if _, dup := t.rules[r.Class]; dup {
			return nil, fmt.Errorf("entitlement: two rules for %s", r.Class)
		}
		t.rules[r.Class] = r
	}
	for _, c := range AllInvestorClasses() {
		if _, ok := t.rules[c]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrNoTDSRule, c)
		}
	}
	return t, nil
}

// For returns the rule for a class.
func (t *TDSTable) For(c InvestorClass) (TDSRule, error) {
	r, ok := t.rules[c]
	if !ok {
		return TDSRule{}, fmt.Errorf("%w: %s", ErrNoTDSRule, c)
	}
	return r, nil
}

// SampleTDSTable is a starting point for development, not tax advice.
//
// # Read this before using it for anything real
//
// The resident rate of 10% reflects the statutory rate for distributed income of a business trust
// paid to a resident unitholder, which is the well-documented part. Everything else here is a
// placeholder:
//
//   - The non-resident treatment is genuinely more complicated than one number. It varies by the
//     nature of the distributed income, by treaty, and by whether a treaty benefit has been claimed
//     with the paperwork to support it. The 10% here is a placeholder and is very likely wrong for a
//     real NRI holder.
//   - The section references need confirming against whichever Act applies to the relevant financial
//     year, given the 2025 renumbering.
//
// This must be replaced with a table signed off by a chartered accountant before a rupee moves. It
// exists so the arithmetic can be tested, not so the rates can be trusted.
func SampleTDSTable() *TDSTable {
	t, err := NewTDSTable(
		TDSRule{Class: ClassResidentIndividual, Section: "194LBA(1)", RateBps: 1000},
		TDSRule{Class: ClassHUF, Section: "194LBA(1)", RateBps: 1000},
		TDSRule{Class: ClassBodyCorporate, Section: "194LBA(1)", RateBps: 1000},
		TDSRule{Class: ClassNRI, Section: "194LBA(2)", RateBps: 1000},
	)
	if err != nil {
		panic(err) // The literal above is fixed; a failure here is a programming error.
	}
	return t
}

// TaxDeduction is the withholding computed for one entitlement.
type TaxDeduction struct {
	Class   InvestorClass
	Section string
	RateBps uint16

	AmountPaise     money.Paise
	NetPayablePaise money.Paise

	Form15GHOnFile        bool
	LowerDeductionCertRef string
}

// ComputeTDS applies a rule to a gross amount.
//
// # Why the deduction rounds up
//
// The exact withholding is gross × rate / 10000, which is rarely a whole number of paise, so one of
// the two parties absorbs a fraction. Rounding down under-deducts, and an under-deduction is the
// deductor's problem: it carries interest and penalty, and it is discovered by the tax authority
// rather than by us. Rounding up over-deducts by at most one paise, which the holder reclaims through
// their return.
//
// Net is then gross minus the deduction, so the two always reconstruct the gross exactly and no paise
// is created or lost between the anchored figure and what is paid.
//
// Note that market practice is to round withholding to the nearest rupee rather than the nearest
// paise. That is a policy choice with a visible effect on every payment and it needs a chartered
// accountant's sign-off, so it is not assumed here.
func ComputeTDS(gross money.Paise, rule TDSRule, opts DeductionOptions) (TaxDeduction, error) {
	if gross < 0 {
		return TaxDeduction{}, fmt.Errorf("entitlement: gross cannot be negative, got %d", gross)
	}
	if err := rule.Validate(); err != nil {
		return TaxDeduction{}, err
	}

	rate := rule.RateBps
	section := rule.Section

	// A lower-deduction certificate replaces the statutory rate, and a Form 15G or 15H declaration
	// removes the deduction. Both require the supporting reference to be recorded, because a reduced
	// deduction with no paperwork behind it is the same exposure as no deduction at all.
	switch {
	case opts.Form15GHOnFile:
		if !rule.Class.IsResident() {
			return TaxDeduction{}, fmt.Errorf(
				"entitlement: a Form 15G/15H declaration does not apply to %s", rule.Class)
		}
		rate = 0

	case opts.LowerDeductionCertRef != "":
		if opts.LowerDeductionRateBps == nil {
			return TaxDeduction{}, errors.New(
				"entitlement: a lower-deduction certificate reference was given without a rate")
		}
		if *opts.LowerDeductionRateBps > rule.RateBps {
			return TaxDeduction{}, fmt.Errorf(
				"entitlement: certificate rate %d bps exceeds the statutory %d bps",
				*opts.LowerDeductionRateBps, rule.RateBps)
		}
		rate = *opts.LowerDeductionRateBps

	case opts.LowerDeductionRateBps != nil:
		return TaxDeduction{}, errors.New(
			"entitlement: a lower-deduction rate was given with no certificate reference")
	}

	amount, err := ceilMulDiv(gross, int64(rate), 10_000)
	if err != nil {
		return TaxDeduction{}, err
	}
	if amount > gross {
		return TaxDeduction{}, fmt.Errorf(
			"entitlement: computed withholding %d exceeds gross %d", amount, gross)
	}

	net, err := gross.Sub(amount)
	if err != nil {
		return TaxDeduction{}, err
	}

	return TaxDeduction{
		Class:                 rule.Class,
		Section:               section,
		RateBps:               rate,
		AmountPaise:           amount,
		NetPayablePaise:       net,
		Form15GHOnFile:        opts.Form15GHOnFile,
		LowerDeductionCertRef: opts.LowerDeductionCertRef,
	}, nil
}

// DeductionOptions carries per-holder overrides to the statutory rate.
type DeductionOptions struct {
	Form15GHOnFile        bool
	LowerDeductionCertRef string
	LowerDeductionRateBps *uint16
}

// ceilMulDiv returns ceil(p * num / den) exactly.
//
// Routed through MulDivFloor, which uses big.Int internally, so the intermediate product cannot
// overflow regardless of the amount.
func ceilMulDiv(p money.Paise, num, den int64) (money.Paise, error) {
	if p == 0 || num == 0 {
		return 0, nil
	}
	q, rem, err := p.MulDivFloor(num, den)
	if err != nil {
		return 0, err
	}
	if rem.Sign() != 0 {
		if q, err = q.Add(1); err != nil {
			return 0, err
		}
	}
	return q, nil
}
