// Package ndcf derives net distributable cash flow from line items.
//
// # The thing this package exists to get right
//
// SEBI's distribution floor applies to net distributable cash flow, not to rent collected. A system
// that computes 95% of gross rent distributes the wrong amount, and it errs high, which means paying
// out money the scheme needed for property tax and statutory reserves. That is not a rounding
// disagreement, it is a solvency problem discovered a quarter later.
//
// So the deduction chain is explicit and line by line, every line carries an evidence digest, and
// NDCF is only ever a computed result. There is no setter for it.
package ndcf

import (
	"errors"
	"fmt"
)

// LineType is one entry in the NDCF derivation.
type LineType string

const (
	// Inflows.
	LineGrossRent      LineType = "GROSS_RENT"
	LineCAMRecovery    LineType = "CAM_RECOVERY"
	LineInterestIncome LineType = "INTEREST_INCOME"

	// Operating outflows.
	LinePropertyTax LineType = "PROPERTY_TAX"
	LineInsurance   LineType = "INSURANCE"
	LineMaintenance LineType = "MAINTENANCE"

	// Fee outflows.
	LineTrusteeFee LineType = "TRUSTEE_FEE"
	LineIMFee      LineType = "IM_FEE"
	LineValuerFee  LineType = "VALUER_FEE"
	LineAuditFee   LineType = "AUDIT_FEE"

	// Reserve outflows. Watched closely; see ReserveRatioBps.
	LineStatutoryReserve      LineType = "STATUTORY_RESERVE"
	LineWorkingCapitalReserve LineType = "WORKING_CAPITAL_RESERVE"

	// LineDebtService exists so the Go vocabulary matches the Postgres enum exactly. A v1
	// unleveraged scheme has no debt and never emits it, but a type present in the database and
	// absent here would fail to round-trip a row the database considers valid.
	LineDebtService LineType = "DEBT_SERVICE"

	LineOther LineType = "OTHER"
)

// Direction is the sign of a line item's effect on NDCF.
type Direction string

const (
	Inflow  Direction = "INFLOW"
	Outflow Direction = "OUTFLOW"
)

var ErrUnknownLineType = errors.New("ndcf: unknown line type")

// directions maps each line type to its fixed direction.
//
// The mapping is total and fixed, mirroring the Postgres CHECK constraint that ties direction to
// type. The reason both exist is the failure they prevent: a PROPERTY_TAX row recorded as an INFLOW
// would inflate NDCF, and the resulting distribution would be larger than the scheme can afford,
// with the shortfall appearing as an unexplained cash gap later.
var directions = map[LineType]Direction{
	LineGrossRent:      Inflow,
	LineCAMRecovery:    Inflow,
	LineInterestIncome: Inflow,

	LinePropertyTax:           Outflow,
	LineInsurance:             Outflow,
	LineMaintenance:           Outflow,
	LineTrusteeFee:            Outflow,
	LineIMFee:                 Outflow,
	LineValuerFee:             Outflow,
	LineAuditFee:              Outflow,
	LineStatutoryReserve:      Outflow,
	LineWorkingCapitalReserve: Outflow,
	LineDebtService:           Outflow,
	LineOther:                 Outflow,
}

// AllLineTypes lists every line type in a stable order.
func AllLineTypes() []LineType {
	return []LineType{
		LineGrossRent, LineCAMRecovery, LineInterestIncome,
		LinePropertyTax, LineInsurance, LineMaintenance,
		LineTrusteeFee, LineIMFee, LineValuerFee, LineAuditFee,
		LineStatutoryReserve, LineWorkingCapitalReserve,
		LineDebtService, LineOther,
	}
}

// Valid reports whether the line type is recognised.
func (lt LineType) Valid() bool {
	_, ok := directions[lt]
	return ok
}

// Direction returns the fixed direction for the line type.
//
// A function, not a field on LineItem. If direction were stored alongside the amount, a caller could
// supply a value inconsistent with the type, and the code would then need to decide which of the two
// to trust. Deriving it removes the question: there is no way to express a PROPERTY_TAX inflow.
func (lt LineType) Direction() (Direction, error) {
	d, ok := directions[lt]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownLineType, lt)
	}
	return d, nil
}

// MustDirection is Direction for line types already known to be valid.
func (lt LineType) MustDirection() Direction {
	d, err := lt.Direction()
	if err != nil {
		panic(err)
	}
	return d
}

// IsInflow reports whether the line type increases NDCF.
func (lt LineType) IsInflow() bool { return directions[lt] == Inflow }

// IsReserve reports whether the line type is a retention rather than a payment out.
//
// Separated because reserves are the one deduction class with no external counterparty. A property
// tax payment is evidenced by a receipt from a municipal body; a reserve is evidenced by a decision.
// That makes reserves the obvious lever for suppressing NDCF, so they are totalled separately and
// reported as a ratio rather than mixed into the general outflow figure.
func (lt LineType) IsReserve() bool {
	return lt == LineStatutoryReserve || lt == LineWorkingCapitalReserve
}

// IsFee reports whether the line type is a fee to a service provider.
func (lt LineType) IsFee() bool {
	switch lt {
	case LineTrusteeFee, LineIMFee, LineValuerFee, LineAuditFee:
		return true
	default:
		return false
	}
}
