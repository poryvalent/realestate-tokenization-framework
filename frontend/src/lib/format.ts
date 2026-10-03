/** Money is integer paise everywhere. Format for display only. */
export function inr(paise: number | null | undefined): string {
  if (paise === null || paise === undefined || !Number.isFinite(paise)) return '—';
  return new Intl.NumberFormat('en-IN', {
    style: 'currency',
    currency: 'INR',
    maximumFractionDigits: paise % 100 === 0 ? 0 : 2,
  }).format(paise / 100);
}

export function inrShort(paise: number | null | undefined): string {
  if (paise === null || paise === undefined || !Number.isFinite(paise)) return '—';
  const rupees = paise / 100;
  if (rupees >= 1e7) return `₹${(rupees / 1e7).toFixed(2)} Cr`;
  if (rupees >= 1e5) return `₹${(rupees / 1e5).toFixed(2)} L`;
  if (rupees >= 1e3) return `₹${(rupees / 1e3).toFixed(2)}k`;
  return inr(paise);
}

export function int(n: number | null | undefined): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return '—';
  return new Intl.NumberFormat('en-IN').format(n);
}

/** RFC 3339 instant -> IST-aware display. */
export function dateTime(ts: string | null | undefined): string {
  if (!ts) return '—';
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return String(ts);
  return new Intl.DateTimeFormat('en-IN', {
    dateStyle: 'medium',
    timeStyle: 'short',
    timeZone: 'Asia/Kolkata',
  }).format(d);
}

/** Calendar dates (YYYY-MM-DD) must not go through timezone conversion. */
export function calDate(d: string | null | undefined): string {
  if (!d) return '—';
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(d);
  if (!m) return String(d);
  const months = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  return `${Number(m[3])} ${months[Number(m[2]) - 1]} ${m[1]}`;
}

export function shortHex(h: string | null | undefined, chars = 10): string {
  if (!h) return '—';
  const s = String(h).toLowerCase();
  if (s.length <= chars + 3) return s;
  return `${s.slice(0, chars)}…${s.slice(-4)}`;
}

export function demandRatio(num: number | null | undefined, den: number | null | undefined): string {
  if (num === null || num === undefined || !den) return '—';
  return `${(num / den).toFixed(2)}×`;
}

const OFFER_STATUS_LABEL: Record<string, string> = {
  CONFIGURED: 'Configured',
  OPEN: 'Open for bids',
  CLOSED: 'Closed',
  BOOK_FROZEN: 'Book frozen',
  SEED_COMMITTED: 'Seed committed',
  SEED_REVEALED: 'Seed revealed',
  BALLOT_DRAWN: 'Ballot drawn',
  ALLOTMENT_FINALISED: 'Allotment finalised',
  SETTLEMENT_STARTED: 'Settlement started',
  SETTLED: 'Settled',
  PAYOUTS_DUE: 'Payouts due',
  DISTRIBUTED: 'Distributed',
  ABORTED: 'Aborted',
};

export function offerStatusLabel(s: string | null | undefined): string {
  if (!s) return '—';
  return OFFER_STATUS_LABEL[s] ?? s.replace(/_/g, ' ').toLowerCase();
}

/** Tone drives the StatusPill colour. Terminal + attention states only. */
export function statusTone(kind: 'offer' | 'bid' | 'payout' | 'period' | 'outbox', s: string): 'ok' | 'warn' | 'bad' | 'info' | 'neutral' {
  void kind;
  if (/^(SETTLED|DISTRIBUTED|ALLOTMENT_FINALISED|SETTLED|COMPLETED|CONFIRMED|ANCHORED|UNITS_CREDITED|FUNDS_DEBITED|VERIFIED|CLOSED|ACTIVE)$/.test(s)) return 'ok';
  if (/^(ABORTED|REJECTED_BALLOT|REJECTED_TECHNICAL|FAILED|REVERSED|EXPIRED|CONFLICT|BLOCKED_PAYOUT)$/.test(s)) return 'bad';
  if (/^(OPEN|PENDING|SUBMITTED|BLOCK_REQUESTED|IN_BOOK|VALIDATED|FUNDS_BLOCKED|QUEUED|SENDING|CONFIGURED)$/.test(s)) return 'info';
  if (/^(PAUSED|ESCALATED|RECOMMIT|UNBLOCK_REQUESTED|DEBIT_INSTRUCTED|PROCESSING)$/.test(s)) return 'warn';
  return 'neutral';
}
