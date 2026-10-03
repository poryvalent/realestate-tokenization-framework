import { useRef, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { motion } from 'framer-motion';
import { ArrowLeft, Landmark, ShieldCheck, Wallet } from 'lucide-react';
import { useSession } from '../api/session';
import { usePoll } from '../api/usePoll';
import { apiFetch, newIdempotencyKey, ApiError } from '../api/client';
import type { Bid, Me, Offer } from '../api/types';
import { inr, int } from '../lib/format';
import { Card, LoadingCard, ErrorBox, Empty } from '../components/ui';
import { useToast } from '../components/Toast';

const inputCls =
  'w-full rounded-xl border border-white/15 bg-white/[0.04] px-4 py-2.5 text-sm text-white placeholder:text-white/30 outline-none transition focus:border-brand-blue/60 focus:bg-white/[0.06] focus:ring-2 focus:ring-brand-blue/20 tabular-nums';

export default function PlaceBid() {
  const { id } = useParams();
  const { token, mockRole, signedIn } = useSession();
  const toast = useToast();
  const nav = useNavigate();
  const offer = usePoll<Offer>(id ? `/offers/${id}` : null, {});
  const me = usePoll<Me>(signedIn ? '/me' : null, { token });

  const [units, setUnits] = useState('2');
  const [priceLakh, setPriceLakh] = useState('10');
  const [demat, setDemat] = useState('');
  const [bank, setBank] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [badFields, setBadFields] = useState<Set<string>>(new Set());
  const idemRef = useRef<string | null>(null);

  const flagInvalid = (inputId: string) =>
    setBadFields((prev) => new Set(prev).add(inputId));
  const clearInvalid = (inputId: string) =>
    setBadFields((prev) => {
      if (!prev.has(inputId)) return prev;
      const next = new Set(prev);
      next.delete(inputId);
      return next;
    });
  const errRing = (f: string) =>
    badFields.has(f) ? ' !border-rose-300/60 !ring-2 !ring-rose-300/20' : '';

  if (!signedIn) {
    return (
      <div className="mx-auto max-w-2xl">
        <motion.h1
          initial={{ opacity: 0, y: 24, filter: 'blur(6px)' }}
          animate={{ opacity: 1, y: 0, filter: 'blur(0px)' }}
          transition={{ duration: 0.6, ease: [0.25, 0.1, 0.25, 1] }}
          className="text-gradient text-4xl font-semibold tracking-tight sm:text-5xl"
        >
          Place a bid
        </motion.h1>
        <div className="mt-6">
          <Empty
            title="Sign in first"
            body="Bids are per-investor and need a signed-in session."
            action={
              <Link
                to="/login"
                className="rounded-full bg-white px-5 py-2 text-sm font-semibold text-[#020319] transition hover:bg-white/85"
              >
                Sign in
              </Link>
            }
          />
        </div>
      </div>
    );
  }
  if (offer.loading) return <LoadingCard label="Loading offer" />;
  if (offer.error) return <ErrorBox error={offer.error} retry={offer.refresh} />;
  if (!offer.data) return <Empty title="Offer not found" />;
  const o = offer.data;
  if (o.status !== 'OPEN') {
    return (
      <div className="mx-auto max-w-2xl">
        <Link
          to={`/offers/${id}`}
          className="inline-flex items-center gap-1.5 text-sm text-brand-mist hover:underline"
        >
          <ArrowLeft className="h-3.5 w-3.5" /> Back to offer
        </Link>
        <motion.h1
          initial={{ opacity: 0, y: 24, filter: 'blur(6px)' }}
          animate={{ opacity: 1, y: 0, filter: 'blur(0px)' }}
          transition={{ duration: 0.6, ease: [0.25, 0.1, 0.25, 1] }}
          className="text-gradient mt-3 text-4xl font-semibold tracking-tight"
        >
          Place a bid
        </motion.h1>
        <div className="mt-6 rounded-2xl border border-amber-300/30 bg-amber-300/10 p-5 text-sm text-amber-100">
          This offer is no longer open and does not accept bids.
        </div>
      </div>
    );
  }
  const t = o.terms;
  const demats = me.data?.dematAccounts ?? [];
  const banks = me.data?.bankAccounts ?? [];

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    const unitsBid = Number(units);
    const pricePerUnitPaise = Math.round(Number(priceLakh) * 100000 * 100);
    let bad = false;
    if (!Number.isInteger(unitsBid) || unitsBid < (t?.minBidUnits ?? 1) || unitsBid > (t?.maxBidUnits ?? 1)) { flagInvalid('units'); bad = true; }
    if (!Number.isFinite(pricePerUnitPaise) || pricePerUnitPaise < (t?.priceBandLowerPaise ?? 0) || pricePerUnitPaise > (t?.priceBandUpperPaise ?? 0)) { flagInvalid('price'); bad = true; }
    if (!demat) { flagInvalid('demat'); bad = true; }
    if (!bank) { flagInvalid('bank'); bad = true; }
    if (bad) return;

    // One idempotency key per user intent: generated once, reused across retries.
    if (!idemRef.current) idemRef.current = newIdempotencyKey();
    setSubmitting(true);
    try {
      const res = await apiFetch<Bid>(`/offers/${id}/bids`, {
        method: 'POST',
        token,
        mockRole: mockRole || undefined,
        idempotencyKey: idemRef.current,
        body: { unitsBid, pricePerUnitPaise, dematAccountId: demat, bankAccountId: bank },
      });
      toast(res.replayed ? 'Already placed — showing the original confirmation.' : `Bid placed. ${inr(res.data.totalAmountPaise)} blocked in your account.`);
      nav('/bids');
    } catch (err) {
      setError(err as ApiError);
    } finally {
      setSubmitting(false);
    }
  };

  const totalPaise = Number(units) * Math.round(Number(priceLakh) * 100000 * 100);

  return (
    <div className="mx-auto max-w-2xl">
      <Link
        to={`/offers/${id}`}
        className="inline-flex items-center gap-1.5 text-sm text-brand-mist hover:underline"
      >
        <ArrowLeft className="h-3.5 w-3.5" /> Offer
      </Link>
      <motion.h1
        initial={{ opacity: 0, y: 24, filter: 'blur(6px)' }}
        animate={{ opacity: 1, y: 0, filter: 'blur(0px)' }}
        transition={{ duration: 0.6, ease: [0.25, 0.1, 0.25, 1] }}
        className="text-gradient mt-3 text-4xl font-semibold tracking-tight sm:text-5xl"
      >
        Place a bid
      </motion.h1>
      <motion.p
        initial={{ opacity: 0, y: 16 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.5, delay: 0.12 }}
        className="mt-4 max-w-xl text-sm leading-relaxed text-white/60"
      >
        Funds are <strong className="font-semibold text-white">blocked in your own bank account, not collected</strong>.
        They are debited only for the units you are allotted; the rest is released. One bid per investor per offer.
      </motion.p>

      <motion.div
        initial={{ opacity: 0, y: 16 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.5, delay: 0.18 }}
        className="mt-6 flex items-start gap-3 rounded-2xl border border-sky-300/20 bg-sky-300/[0.07] p-4 text-sm text-white/70"
      >
        <ShieldCheck className="mt-0.5 h-4 w-4 shrink-0 text-sky-200" />
        <p className="leading-relaxed">
          <span className="font-semibold text-white">ASBA-style block.</span> Your bank reserves the full
          amount; nothing moves to the platform. Debit happens only to the extent of allotment.
        </p>
      </motion.div>

      <div className="mt-6">
        <Card>
          <form onSubmit={submit} noValidate className="space-y-5">
            <div>
              <label htmlFor="units" className="mb-1.5 block text-sm font-medium text-white/80">
                Units <span className="tabular-nums text-white/50">({int(t?.minBidUnits)}–{int(t?.maxBidUnits)})</span>
              </label>
              <input
                id="units"
                className={inputCls + errRing('units')}
                inputMode="numeric"
                value={units}
                onChange={(e) => { setUnits(e.target.value); clearInvalid('units'); }}
              />
              {badFields.has('units') && (
                <p className="mt-1.5 text-xs text-rose-200">Units must be between {int(t?.minBidUnits)} and {int(t?.maxBidUnits)}.</p>
              )}
            </div>
            <div>
              <label htmlFor="price" className="mb-1.5 block text-sm font-medium text-white/80">
                Price per unit, ₹ lakh{' '}
                <span className="tabular-nums text-white/50">({inr(t?.priceBandLowerPaise)} – {inr(t?.priceBandUpperPaise)})</span>
              </label>
              <input
                id="price"
                className={inputCls + errRing('price')}
                inputMode="decimal"
                value={priceLakh}
                onChange={(e) => { setPriceLakh(e.target.value); clearInvalid('price'); }}
              />
              {badFields.has('price') && (
                <p className="mt-1.5 text-xs text-rose-200">Price must sit inside the band.</p>
              )}
            </div>
            <div>
              <label htmlFor="demat" className="mb-1.5 block text-sm font-medium text-white/80">
                Demat account <span className="text-white/50">(units are credited here)</span>
              </label>
              <select
                id="demat"
                className={inputCls + errRing('demat')}
                value={demat}
                onChange={(e) => { setDemat(e.target.value); clearInvalid('demat'); }}
              >
                <option value="">Choose…</option>
                {demats.map((d) => <option key={d.id} value={d.id}>{d.depository} · {d.maskedClientId}{d.verified ? '' : ' (unverified)'}</option>)}
              </select>
              {badFields.has('demat') && (
                <p className="mt-1.5 text-xs text-rose-200">Pick the demat account for this bid.</p>
              )}
            </div>
            <div>
              <label htmlFor="bank" className="mb-1.5 block text-sm font-medium text-white/80">
                Bank account <span className="text-white/50">(block lives here)</span>
              </label>
              <select
                id="bank"
                className={inputCls + errRing('bank')}
                value={bank}
                onChange={(e) => { setBank(e.target.value); clearInvalid('bank'); }}
              >
                <option value="">Choose…</option>
                {banks.map((b) => <option key={b.id} value={b.id}>{b.ifsc} · {b.maskedAccountNumber}{b.verified ? '' : ' (unverified)'}</option>)}
              </select>
              {badFields.has('bank') && (
                <p className="mt-1.5 text-xs text-rose-200">Pick the bank account to block from.</p>
              )}
            </div>
            <div className="flex items-center gap-3 rounded-xl border border-white/10 bg-white/[0.03] px-4 py-3">
              <Wallet className="h-4 w-4 shrink-0 text-brand-mist" />
              <p className="text-sm text-white/70">
                Total to block:{' '}
                <strong className="tabular-nums font-semibold text-white">
                  {Number.isFinite(totalPaise) ? inr(totalPaise) : '—'}
                </strong>
                <span className="text-white/50">, all or nothing — insufficient funds fail the whole block, never a part of it.</span>
              </p>
            </div>
            {error && <ErrorBox error={error} />}
            <button
              type="submit"
              disabled={submitting}
              className="flex w-full items-center justify-center gap-2 rounded-full bg-white px-5 py-2.5 text-sm font-semibold text-[#020319] transition hover:bg-white/85 disabled:opacity-50"
            >
              <Landmark className="h-4 w-4" />
              {submitting ? 'Placing…' : 'Block funds and place bid'}
            </button>
          </form>
        </Card>
      </div>
    </div>
  );
}
