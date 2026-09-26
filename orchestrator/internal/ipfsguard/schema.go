package ipfsguard

// DocType identifies a document class that AcreSync pins to IPFS and anchors
// on-chain by digest.
type DocType string

const (
	// DocSnapshot is the unitholder register frozen at a distribution record
	// date. Its Merkle root is anchored in the period struct and is what
	// verifyEntitlement proves against.
	DocSnapshot DocType = "SNAPSHOT"

	// DocBidbook is the frozen bid book for an offer. Its root is anchored
	// before the seed commitment, which is what makes the ballot auditable.
	DocBidbook DocType = "BIDBOOK"

	// DocNDCFStatement is the line-item derivation of net distributable cash
	// flow for a period, supporting the 95% floor assertion.
	DocNDCFStatement DocType = "NDCF_STATEMENT"

	// DocAllotmentFile is the ballot outcome, reproducible by any third party
	// from the bid book plus the revealed seed.
	DocAllotmentFile DocType = "ALLOTMENT_FILE"
)

// Field declares one allowlisted field.
type Field struct {
	Kind     Kind
	Required bool

	// Enum lists permitted values when Kind is KindEnum.
	Enum []string

	// Object is the nested schema when Kind is KindObject.
	Object Schema

	// Elem describes array elements when Kind is KindArray.
	Elem *Field

	// MaxLen caps array length. Zero means the default cap applies. An
	// unbounded array is a denial-of-service vector against the pinning service
	// and against any verifier that downloads the document.
	MaxLen int
}

// Schema maps a field name to its declaration. Any key present in a document but
// absent from the schema is a validation failure. That is the fail-closed
// property: adding a field to a pinned document requires a deliberate change
// here, reviewed as code, rather than happening as a side effect of a struct
// gaining a member somewhere in the orchestrator.
type Schema map[string]Field

// ndcfLineTypes is the closed set of NDCF derivation line items.
//
// DEBT_SERVICE is present because the enum must match the Postgres type, but a
// v1 unleveraged scheme has no debt and the orchestrator will never emit it.
var ndcfLineTypes = []string{
	"GROSS_RENT", "CAM_RECOVERY", "INTEREST_INCOME",
	"PROPERTY_TAX", "INSURANCE", "MAINTENANCE",
	"TRUSTEE_FEE", "IM_FEE", "VALUER_FEE", "AUDIT_FEE",
	"STATUTORY_RESERVE", "WORKING_CAPITAL_RESERVE",
	"DEBT_SERVICE", "OTHER",
}

// commonHeader is the field set every pinned document carries.
func commonHeader(docType DocType) Schema {
	return Schema{
		"docType":       {Kind: KindEnum, Required: true, Enum: []string{string(docType)}},
		"schemaVersion": {Kind: KindUint32, Required: true},
		"schemeRef":     {Kind: KindBytes32Hex, Required: true},
	}
}

func merge(base Schema, extra Schema) Schema {
	out := make(Schema, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// schemas is the authoritative registry of pinnable document shapes.
var schemas = map[DocType]Schema{

	DocSnapshot: merge(commonHeader(DocSnapshot), Schema{
		"periodId":        {Kind: KindUint32, Required: true},
		"recordDate":      {Kind: KindDateISO, Required: true},
		"takenAt":         {Kind: KindDateTimeISO, Required: true},
		"totalUnits":      {Kind: KindUint32, Required: true},
		"distinctHolders": {Kind: KindUint32, Required: true},
		"merkleRoot":      {Kind: KindBytes32Hex, Required: true},
		"algoVersion":     {Kind: KindUint32, Required: true},
		"lines": {
			Kind: KindArray, Required: true, MaxLen: 10_000,
			Elem: &Field{Kind: KindObject, Object: Schema{
				"leafIndex":      {Kind: KindUint32, Required: true},
				"holder":         {Kind: KindAddress, Required: true},
				"units":          {Kind: KindUint32, Required: true},
				"excluded":       {Kind: KindBool, Required: true},
				"investorAnchor": {Kind: KindBytes32Hex, Required: true},
			}},
		},
	}),

	DocBidbook: merge(commonHeader(DocBidbook), Schema{
		"offerId":         {Kind: KindUUID, Required: true},
		"frozenAt":        {Kind: KindDateTimeISO, Required: true},
		"merkleRoot":      {Kind: KindBytes32Hex, Required: true},
		"leafCount":       {Kind: KindUint32, Required: true},
		"totalUnitsBid":   {Kind: KindUint32, Required: true},
		"distinctBidders": {Kind: KindUint32, Required: true},
		"algoVersion":     {Kind: KindUint32, Required: true},
		"leaves": {
			Kind: KindArray, Required: true, MaxLen: 200_000,
			Elem: &Field{Kind: KindObject, Object: Schema{
				"leafIndex":         {Kind: KindUint32, Required: true},
				"bidRef":            {Kind: KindOpaqueRef, Required: true},
				"investorAnchor":    {Kind: KindBytes32Hex, Required: true},
				"unitsBid":          {Kind: KindUint32, Required: true},
				"pricePerUnitPaise": {Kind: KindUint64, Required: true},
			}},
		},
	}),

	DocNDCFStatement: merge(commonHeader(DocNDCFStatement), Schema{
		"periodId":         {Kind: KindUint32, Required: true},
		"periodStart":      {Kind: KindDateISO, Required: true},
		"periodEnd":        {Kind: KindDateISO, Required: true},
		"currency":         {Kind: KindEnum, Required: true, Enum: []string{"INR"}},
		"ndcfPaise":        {Kind: KindUint64, Required: true},
		"distributedPaise": {Kind: KindUint64, Required: true},
		"distributionBps":  {Kind: KindUint32, Required: true},
		"lineItems": {
			Kind: KindArray, Required: true, MaxLen: 10_000,
			Elem: &Field{Kind: KindObject, Object: Schema{
				"lineType":     {Kind: KindEnum, Required: true, Enum: ndcfLineTypes},
				"direction":    {Kind: KindEnum, Required: true, Enum: []string{"INFLOW", "OUTFLOW"}},
				"amountPaise":  {Kind: KindUint64, Required: true},
				"spvRef":       {Kind: KindUUID},
				"propertyRef":  {Kind: KindUUID},
				"evidenceHash": {Kind: KindBytes32Hex, Required: true},
			}},
		},
	}),

	DocAllotmentFile: merge(commonHeader(DocAllotmentFile), Schema{
		"offerId":           {Kind: KindUUID, Required: true},
		"finalSeed":         {Kind: KindBytes32Hex, Required: true},
		"bidbookRoot":       {Kind: KindBytes32Hex, Required: true},
		"resultRoot":        {Kind: KindBytes32Hex, Required: true},
		"unitsOnOffer":      {Kind: KindUint32, Required: true},
		"unitsAllotted":     {Kind: KindUint32, Required: true},
		"distinctAllottees": {Kind: KindUint32, Required: true},
		"algoVersion":       {Kind: KindUint32, Required: true},
		"allotments": {
			Kind: KindArray, Required: true, MaxLen: 200_000,
			Elem: &Field{Kind: KindObject, Object: Schema{
				"leafIndex":      {Kind: KindUint32, Required: true},
				"investorAnchor": {Kind: KindBytes32Hex, Required: true},
				"unitsAllotted":  {Kind: KindUint32, Required: true},
				"outcome": {Kind: KindEnum, Required: true,
					Enum: []string{"FULL", "PARTIAL", "NIL_BALLOT", "NIL_TECHNICAL"}},
				"ballotRank": {Kind: KindUint32, Required: true},
			}},
		},
	}),
}

// SchemaFor returns the schema for a document type.
func SchemaFor(dt DocType) (Schema, bool) {
	s, ok := schemas[dt]
	return s, ok
}

// DocTypes lists every pinnable document type.
func DocTypes() []DocType {
	return []DocType{DocSnapshot, DocBidbook, DocNDCFStatement, DocAllotmentFile}
}
