/** Structural API types. Field names mirror docs/api/openapi.yaml (verified). */

export interface Scheme {
  id: string;
  name: string;
  investmentManager?: string;
  trustee?: string;
  targetCorpusPaise?: number;
  totalUnits?: number;
  managerUnits?: number;
  unitFaceValuePaise?: number;
  contracts?: Record<string, string>;
  environment?: string;
  [k: string]: unknown;
}

export interface OfferTerms {
  unitsOnOffer: number;
  minBidUnits: number;
  maxBidUnits: number;
  minSubscriptionUnits?: number;
  minDistinctHolders?: number;
  priceBandLowerPaise: number;
  priceBandUpperPaise: number;
  opensAt?: string;
  closesAt?: string;
  allotmentDueAt?: string;
}

export interface Subscription {
  bidCount?: number;
  distinctBidders?: number;
  unitsBid?: number;
  fundsBlockedCount?: number;
  oversubscriptionNumerator?: number;
  oversubscriptionDenominator?: number;
}

export interface Offer {
  id: string;
  schemeId: string;
  offerType?: string;
  status: string;
  terms: OfferTerms;
  subscription?: Subscription;
  createdAt?: string;
}

export interface Readiness {
  currentStatus: string;
  nextExpected: string | null;
  canAdvance: boolean;
  blockers: string[];
  paused?: boolean;
}

export interface AsbaBlock {
  status?: string;
  failureCode?: string | null;
  amountPaise?: number;
}

export interface BidAllotment {
  outcome?: string;
  unitsAllotted?: number;
  amountPayablePaise?: number;
  refundAmountPaise?: number;
}

export interface Bid {
  id: string;
  offerId: string;
  bidRef: string;
  investorAnchor?: string;
  unitsBid: number;
  pricePerUnitPaise: number;
  totalAmountPaise?: number;
  status: string;
  rejectionReason?: string | null;
  block?: AsbaBlock | null;
  allotment?: BidAllotment | null;
  submittedAt?: string;
}

export interface BallotCeremony {
  offerId: string;
  stage: string;
  bidbookRoot?: string | null;
  bidbookCidDigest?: string | null;
  seedCommitment?: string | null;
  targetBlock?: number | null;
  revealWindowBlocks?: number;
  revealDeadlineBlock?: number | null;
  seedPlaintext?: string | null;
  targetBlockHash?: string | null;
  finalSeed?: string | null;
  resultRoot?: string | null;
  attempt: number;
  maxAttempts: number;
  escalated?: boolean;
}

export interface SettlementBatch {
  cursorFrom?: number;
  cursorTo?: number;
  holderCount?: number;
  units?: number;
  submitted?: boolean;
}

export interface SettlementState {
  offerId: string;
  stage: string;
  expectedUnits: number;
  expectedHolders: number;
  creditedUnits: number;
  creditedHolders: number;
  nextStep?: string | null;
  imUnitsRecorded?: boolean;
  batches?: SettlementBatch[];
  finalisation?: { ready?: boolean; failures?: string[] };
}

export interface Period {
  id: string;
  schemeId?: string;
  status?: string;
  recordDate?: string;
  distributionBps?: number;
  floorBps?: number;
  [k: string]: unknown;
}

export interface NdcfLine {
  lineType?: string;
  direction?: string;
  amountPaise?: number;
  label?: string;
}

export interface NdcfStatement {
  periodId?: string;
  grossPaise?: number;
  ndcfPaise?: number;
  distributionBps?: number;
  floorBps?: number;
  lines?: NdcfLine[];
  [k: string]: unknown;
}

export interface InclusionProof {
  leaf?: string;
  proof?: string[];
  root?: string;
  anchoredTx?: string;
  verified?: boolean;
  preimage?: Record<string, unknown>;
  [k: string]: unknown;
}

export interface PublishedAllotment {
  leafIndex?: number;
  investorAnchor?: string;
  outcome?: string;
  unitsAllotted?: number;
}

export interface Me {
  investorId: string;
  displayName?: string;
  investorClass?: string;
  kycStatus?: string;
  panMasked?: string | null;
  walletAddress?: string | null;
  dematAccounts?: { id: string; depository?: string; maskedClientId?: string; verified?: boolean }[];
  bankAccounts?: { id: string; ifsc?: string; maskedAccountNumber?: string; verified?: boolean }[];
}

export interface Holding {
  schemeId: string;
  schemeName?: string;
  units?: number;
}

export interface Entitlement {
  periodId?: string;
  schemeName?: string;
  grossPaise?: number;
  taxWithheldPaise?: number;
  netPayablePaise?: number;
  payable?: boolean;
  units?: number;
}

export interface Payout {
  id?: string;
  periodId?: string;
  amountPaise?: number;
  netPaise?: number;
  status?: string;
  simulated?: boolean;
  provider?: string;
  utr?: string | null;
}

export interface OutboxEntry {
  id?: string;
  kind?: string;
  status?: string;
  schemeId?: string;
  offerId?: string;
  confirmations?: number;
  requiredConfirmations?: number;
  txHash?: string | null;
  createdAt?: string;
}

export interface PublishedDocument {
  documentId?: string;
  filename?: string;
  purpose?: string;
  digest?: string;
  cid?: string;
}
