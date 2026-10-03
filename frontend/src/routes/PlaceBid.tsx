import { useRef, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { useSession } from '../api/session';
import { usePoll } from '../api/usePoll';
import { apiFetch, newIdempotencyKey, ApiError } from '../api/client';
import type { Bid, Me, Offer } from '../api/types';
import { inr, int } from '../lib/format';
import { Card, LoadingCard, ErrorBox, Empty } from '../components/ui';
import { useToast } from '../components/Toast';

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
  const idemRef = useRef<string | null>(null);
  const formRef = useRef<HTMLFormElement>(null);

  if (!signedIn) {
    return (
      <>
        <h1>Place a bid</h1>
        <Empty title="Sign in first" action={<Link className="btn primary" to="/login">Sign in</Link>} />
      </>
    );
  }
  if (offer.loading) return <LoadingCard label="Loading offer" />;
  if (offer.error) return <ErrorBox error={offer.error} retry={offer.refresh} />;
  if (!offer.data) return <Empty title="Offer not found" />;
  const o = offer.data;
  if (o.status !== 'OPEN') {
    return (
      <>
        <h1>Place a bid</h1>
        <div className="alert warn"><p>This offer is no longer open and does not accept bids.</p></div>
        <p><Link to={`/offers/${id}`}>← Back to offer</Link></p>
      </>
    );
  }
  const t = o.terms;
  const demats = me.data?.dematAccounts ?? [];
  const banks = me.data?.bankAccounts ?? [];

  const flagInvalid = (inputId: string) => {
    // t-input recipe: replay the shake from a clean baseline (remove → reflow → add).
    // is-error belongs on the inner .t-input-wrap: that is what reveals .t-error-msg.
    const wrap = formRef.current?.querySelector(`[data-wrap="${inputId}"] .t-input-wrap`);
    const input = wrap?.querySelector('.t-input');
    if (!wrap || !input) return;
    wrap.classList.add('is-error');
    input.classList.remove('is-shaking');
    void (input as HTMLElement).offsetWidth;
    input.classList.add('is-shaking');
  };

  const clearInvalid = (inputId: string) => {
    const wrap = formRef.current?.querySelector(`[data-wrap="${inputId}"] .t-input-wrap`);
    wrap?.classList.remove('is-error');
  };

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
    <>
      <p style={{ marginTop: 24 }}><Link to={`/offers/${id}`}>← Offer</Link></p>
      <div className="narrow">
      <h1>Place a bid</h1>
      <p>
        Funds are <strong>blocked in your own bank account, not collected</strong>. They are debited
        only for the units you are allotted; the rest is released. One bid per investor per offer.
      </p>
      <Card>
        <form ref={formRef} onSubmit={submit} noValidate>
          <div className="field" data-wrap="units">
            <div className="t-input-wrap">
              <label htmlFor="units">Units ({int(t?.minBidUnits)}–{int(t?.maxBidUnits)})</label>
              <div className="t-input"><input id="units" className="input num" inputMode="numeric" value={units} onChange={(e) => { setUnits(e.target.value); clearInvalid('units'); }} /></div>
              <p className="t-error-msg">Units must be between {int(t?.minBidUnits)} and {int(t?.maxBidUnits)}.</p>
            </div>
          </div>
          <div className="field" data-wrap="price">
            <div className="t-input-wrap">
              <label htmlFor="price">Price per unit, ₹ lakh ({inr(t?.priceBandLowerPaise)} – {inr(t?.priceBandUpperPaise)})</label>
              <div className="t-input"><input id="price" className="input num" inputMode="decimal" value={priceLakh} onChange={(e) => { setPriceLakh(e.target.value); clearInvalid('price'); }} /></div>
              <p className="t-error-msg">Price must sit inside the band.</p>
            </div>
          </div>
          <div className="field" data-wrap="demat">
            <div className="t-input-wrap">
              <label htmlFor="demat">Demat account (units are credited here)</label>
              <div className="t-input">
                <select id="demat" className="input" value={demat} onChange={(e) => { setDemat(e.target.value); clearInvalid('demat'); }}>
                  <option value="">Choose…</option>
                  {demats.map((d) => <option key={d.id} value={d.id}>{d.depository} · {d.maskedClientId}{d.verified ? '' : ' (unverified)'}</option>)}
                </select>
              </div>
              <p className="t-error-msg">Pick the demat account for this bid.</p>
            </div>
          </div>
          <div className="field" data-wrap="bank">
            <div className="t-input-wrap">
              <label htmlFor="bank">Bank account (block lives here)</label>
              <div className="t-input">
                <select id="bank" className="input" value={bank} onChange={(e) => { setBank(e.target.value); clearInvalid('bank'); }}>
                  <option value="">Choose…</option>
                  {banks.map((b) => <option key={b.id} value={b.id}>{b.ifsc} · {b.maskedAccountNumber}{b.verified ? '' : ' (unverified)'}</option>)}
                </select>
              </div>
              <p className="t-error-msg">Pick the bank account to block from.</p>
            </div>
          </div>
          <p>Total to block: <strong className="num">{Number.isFinite(totalPaise) ? inr(totalPaise) : '—'}</strong>, all or nothing — insufficient funds fail the whole block, never a part of it.</p>
          {error && <ErrorBox error={error} />}
          <button type="submit" className="btn primary" disabled={submitting}>
            {submitting ? 'Placing…' : 'Block funds and place bid'}
          </button>
        </form>
      </Card>
      </div>
    </>
  );
}
