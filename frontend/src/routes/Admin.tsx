import { useEffect, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { useSession } from '../api/session';
import { usePoll } from '../api/usePoll';
import { apiFetch, newIdempotencyKey, pageItems, ApiError } from '../api/client';
import type { BallotCeremony, Offer, OutboxEntry, Readiness, SettlementState } from '../api/types';
import { int } from '../lib/format';
import { Card, LoadingCard, ErrorBox, StatusPill, Empty, Hash, EtherscanLink } from '../components/ui';
import { useToast } from '../components/Toast';

const LS_OFFER = 'acresync.adminOffer';

function useAdminAuth() {
  return useSession();
}

async function postAdmin<T>(
  path: string,
  token: string | null,
  mockRole: string,
  body?: unknown,
): Promise<T> {
  const res = await apiFetch<T>(path, {
    method: 'POST',
    token,
    mockRole: mockRole || undefined,
    idempotencyKey: newIdempotencyKey(),
    body: body ?? {},
  });
  return res.data;
}

function ActionButton({
  label,
  path,
  body,
  disabled,
  confirm,
}: {
  label: string;
  path: string;
  body?: unknown;
  disabled?: boolean;
  confirm?: string;
}) {
  const { token, mockRole } = useAdminAuth();
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const run = async () => {
    if (confirm && !window.confirm(confirm)) return;
    setBusy(true);
    setError(null);
    try {
      await postAdmin(path, token, mockRole, body);
      toast(`${label} — accepted. Polling will show the new state.`);
      // polls refresh on their 3s tick; nudge by reload of ETag state via location-agnostic event
      window.dispatchEvent(new CustomEvent('acresync:mutated'));
    } catch (e) {
      setError(e as ApiError);
    } finally {
      setBusy(false);
    }
  };
  return (
    <div>
      <button type="button" className="btn small" disabled={disabled || busy} onClick={run}>
        {busy ? 'Working…' : label}
      </button>
      {error && <ErrorBox error={error} operator />}
    </div>
  );
}

function ReadinessPanel({ offerId }: { offerId: string }) {
  const { token, mockRole } = useSession();
  const r = usePoll<Readiness>(`/admin/offers/${offerId}/readiness`, {
    intervalMs: 3000,
    token,
    mockRole: mockRole || undefined,
  });

  useEffect(() => {
    const onMut = () => r.refresh();
    window.addEventListener('acresync:mutated', onMut);
    return () => window.removeEventListener('acresync:mutated', onMut);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [offerId]);

  if (r.loading) return <LoadingCard lines={3} label="Loading readiness" />;
  if (r.error) return <ErrorBox error={r.error} retry={r.refresh} operator />;
  if (!r.data) return <Empty title="No readiness" />;
  const d = r.data;
  return (
    <Card>
      <h3 style={{ marginTop: 0 }}>Readiness — act on this, not on buttons</h3>
      <dl className="kv">
        <dt>Current</dt>
        <dd><StatusPill kind="offer" status={d.currentStatus} /></dd>
        <dt>Next expected</dt>
        <dd>{d.nextExpected ? <StatusPill kind="offer" status={d.nextExpected} /> : '— (terminal)'}</dd>
        <dt>Can advance</dt>
        <dd>{d.canAdvance ? 'yes' : 'no'}</dd>
        {d.paused && <><dt>Paused</dt><dd>yes — everything except abort is refused (423)</dd></>}
      </dl>
      {(d.blockers ?? []).length > 0 && (
        <>
          <h3>Blockers ({d.blockers.length})</h3>
          <ul>
            {d.blockers.map((b, i) => (
              <li key={i}>{b}</li>
            ))}
          </ul>
        </>
      )}
      {d.nextExpected ? (
        <ActionButton
          label={d.canAdvance ? `Advance to ${d.nextExpected}` : `Next: ${d.nextExpected} (blocked — see above)`}
          path={`/admin/offers/${offerId}/transitions`}
          body={{ to: d.nextExpected }}
          disabled={!d.canAdvance}
        />
      ) : (
        <p>Terminal state — nothing further to advance.</p>
      )}
      <p className="hint" style={{ fontSize: '0.83rem', color: 'var(--ink-3)' }}>
        409s (<span className="mono">anchor_not_confirmed</span>) are waiting states, not errors — confirmations land a few seconds after each anchor, then the step succeeds on retry.
      </p>
    </Card>
  );
}

function CeremonyPanel({ offerId }: { offerId: string }) {
  const c = usePoll<BallotCeremony>(`/offers/${offerId}/ballot`, { intervalMs: 8000 });
  return (
    <Card>
      <h3 style={{ marginTop: 0 }}>Ballot ceremony</h3>
      {c.data && (
        <p>
          Attempt <strong className="num">{c.data.attempt}</strong> of <strong className="num">{c.data.maxAttempts}</strong>
          {c.data.escalated ? ' · escalated — awaiting trustee (a second party), not a permission issue' : ''} · Stage <strong>{c.data.stage}</strong>
        </p>
      )}
      <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
        <ActionButton label="Freeze book" path={`/admin/offers/${offerId}/book/freeze`} />
        <ActionButton label="Commit seed" path={`/admin/offers/${offerId}/ballot/commit`} />
        <ActionButton label="Reveal seed" path={`/admin/offers/${offerId}/ballot/reveal`} />
        <ActionButton label="Recommit (window lapsed only)" path={`/admin/offers/${offerId}/ballot/recommit`} />
        <ActionButton label="Draw ballot" path={`/admin/offers/${offerId}/ballot/draw`} />
      </div>
      <p style={{ fontSize: '0.85rem', color: 'var(--ink-3)' }}>
        Ordering is enforced server-side (commit → reveal → draw). <span className="mono">recommit</span> only after the reveal window lapses,
        capped at <span className="mono">maxAttempts</span> — after that a trustee must escalate. Each step after the freeze returns{' '}
        <span className="mono">409 anchor_not_confirmed</span> for ~5s, then succeeds on retry.
      </p>
      <p style={{ marginBottom: 0 }}><Link to={`/offers/${offerId}/ballot`}>Public ceremony view →</Link></p>
    </Card>
  );
}

function SettlementPanel({ offerId }: { offerId: string }) {
  const { token, mockRole } = useSession();
  const s = usePoll<SettlementState>(`/admin/offers/${offerId}/settlement`, {
    intervalMs: 3000,
    token,
    mockRole: mockRole || undefined,
  });
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);

  useEffect(() => {
    const onMut = () => s.refresh();
    window.addEventListener('acresync:mutated', onMut);
    return () => window.removeEventListener('acresync:mutated', onMut);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [offerId]);

  const submitBatch = async () => {
    if (!s.data) return;
    setBusy(true);
    setError(null);
    try {
      await postAdmin(`/admin/offers/${offerId}/settlement/batches`, token, mockRole, {
        cursorFrom: s.data.creditedHolders,
      });
      toast('Batch accepted. Cursor advances on confirmation.');
      window.dispatchEvent(new CustomEvent('acresync:mutated'));
      s.refresh();
    } catch (e) {
      setError(e as ApiError);
    } finally {
      setBusy(false);
    }
  };

  if (s.loading) return <LoadingCard lines={4} label="Loading settlement" />;
  if (s.error) {
    if (s.error.status === 404) {
      return (
        <Card>
          <h3 style={{ marginTop: 0 }}>Settlement</h3>
          <p>Not started. <ActionButton label="Begin settlement" path={`/admin/offers/${offerId}/settlement/begin`} /></p>
          <p style={{ fontSize: '0.85rem', color: 'var(--ink-3)' }}>
            Binds permanently to the anchored ballot result. Covers the public side only (475 of 500 for the reference scheme) —
            the manager's 25 are credited outside the cursor.
          </p>
        </Card>
      );
    }
    return <ErrorBox error={s.error} retry={s.refresh} operator />;
  }
  if (!s.data) return <Empty title="No settlement" />;
  const d = s.data;
  const pct = d.expectedHolders ? Math.min(100, (d.creditedHolders / d.expectedHolders) * 100) : 0;

  return (
    <Card>
      <h3 style={{ marginTop: 0 }}>Settlement — {d.stage}</h3>
      <dl className="kv">
        <dt>Progress</dt>
        <dd className="num">{int(d.creditedHolders)} / {int(d.expectedHolders)} holders · {int(d.creditedUnits)} / {int(d.expectedUnits)} units</dd>
        <dt>Next step</dt>
        <dd className="mono">{d.nextStep ?? 'waiting for confirmations'}</dd>
      </dl>
      <div className="progress" role="progressbar" aria-valuenow={Math.round(pct)} aria-valuemin={0} aria-valuemax={100}>
        <i style={{ transform: `scaleX(${pct / 100})` }} />
      </div>
      {(d.batches ?? []).length > 0 && (
        <div className="table-scroll">
        <table className="ledger" style={{ marginTop: 12 }}>
          <thead><tr><th className="r">From</th><th className="r">To</th><th className="r">Holders</th><th>Submitted</th></tr></thead>
          <tbody>
            {(d.batches ?? []).map((b, i) => (
              <tr key={i}>
                <td className="r num">{int(b.cursorFrom)}</td>
                <td className="r num">{int(b.cursorTo)}</td>
                <td className="r num">{int(b.holderCount)}</td>
                <td>{b.submitted ? 'yes' : 'no'}</td>
              </tr>
            ))}
          </tbody>
        </table>
        </div>
      )}
      {(d.finalisation?.failures ?? []).length > 0 && (
        <>
          <h3>Finalisation failures ({d.finalisation!.failures!.length}) — all of them, not just the first</h3>
          <ul>
            {d.finalisation!.failures!.map((f, i) => (
              <li key={i}>{f}</li>
            ))}
          </ul>
        </>
      )}
      <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginTop: 12 }}>
        <ActionButton label="Begin settlement" path={`/admin/offers/${offerId}/settlement/begin`} disabled={d.stage !== 'NOT_STARTED'} />
        <div>
          <button type="button" className="btn small" disabled={busy || d.stage !== 'IN_PROGRESS'} onClick={submitBatch}>
            {busy ? 'Submitting…' : `Submit next batch (cursorFrom=${int(d.creditedHolders)})`}
          </button>
        </div>
        <ActionButton label="Finalise settlement" path={`/admin/offers/${offerId}/settlement/finalise`} disabled={d.stage !== 'IN_PROGRESS'} />
      </div>
      {error && <ErrorBox error={error} operator />}
      <p style={{ fontSize: '0.85rem', color: 'var(--ink-3)' }}>
        <span className="mono">cursorFrom</span> must equal holders already credited, exactly. A batch that ran out of gas is retryable verbatim;
        a batch that succeeded is rejected by both key and cursor. <span className="mono">nextStep: null</span> means wait for the queued call to confirm.
      </p>
    </Card>
  );
}

function OutboxPanel({ schemeId }: { schemeId: string | null }) {
  const { token, mockRole } = useSession();
  const o = usePoll<OutboxEntry[] | { items: OutboxEntry[] }>(
    schemeId ? `/admin/outbox?schemeId=${encodeURIComponent(schemeId)}&limit=100` : null,
    { intervalMs: 5000, token, mockRole: mockRole || undefined },
  );
  if (!schemeId) return null;
  const rows = pageItems(o.data);
  return (
    <Card>
      <h3 style={{ marginTop: 0 }}>Chain outbox — queued is not sent</h3>
      {o.loading && <LoadingCard lines={2} label="Loading outbox" />}
      {o.error && <ErrorBox error={o.error} retry={o.refresh} operator />}
      {!o.loading && !o.error && rows.length === 0 && <p>No queued chain calls for this scheme.</p>}
      {rows.length > 0 && (
        <div className="table-scroll">
        <table className="ledger">
          <thead><tr><th>Kind</th><th>Status</th><th className="r">Confirmations</th><th>Tx</th></tr></thead>
          <tbody>
            {rows.map((e, i) => (
              <tr key={e.id ?? i}>
                <td className="mono">{e.kind ?? '—'}</td>
                <td><StatusPill kind="outbox" status={e.status ?? 'UNKNOWN'} /></td>
                <td className="r num">{int(e.confirmations)} / {int(e.requiredConfirmations)}</td>
                <td><EtherscanLink tx={e.txHash} /></td>
              </tr>
            ))}
          </tbody>
        </table>
        </div>
      )}
      <p style={{ fontSize: '0.85rem', color: 'var(--ink-3)', marginBottom: 0 }}>
        Chain calls confirm asynchronously — the queue below tracks every call to depth before the next step unlocks.
      </p>
    </Card>
  );
}

function CreateOffer({ defaultSchemeId, onCreated }: { defaultSchemeId?: string; onCreated: (id: string) => void }) {
  const { token, mockRole } = useSession();
  const toast = useToast();
  const [schemeId, setSchemeId] = useState(defaultSchemeId ?? '');
  const [offerType, setOfferType] = useState('INITIAL');
  const [units, setUnits] = useState('475');
  const [minBid, setMinBid] = useState('1');
  const [maxBid, setMaxBid] = useState('25');
  const [lowLakh, setLowLakh] = useState('10');
  const [highLakh, setHighLakh] = useState('10.5');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const now = new Date();
      const opensAt = new Date(now.getTime() - 86400000).toISOString();
      const closesAt = new Date(now.getTime() + 14 * 86400000).toISOString();
      const allotmentDueAt = new Date(now.getTime() + 21 * 86400000).toISOString();
      const res = await apiFetch<Offer>('/admin/offers', {
        method: 'POST',
        token,
        mockRole: mockRole || undefined,
        idempotencyKey: newIdempotencyKey(),
        body: {
          schemeId,
          offerType,
          terms: {
            unitsOnOffer: Number(units),
            minBidUnits: Number(minBid),
            maxBidUnits: Number(maxBid),
            minSubscriptionUnits: Math.floor(Number(units) * 0.9),
            minDistinctHolders: 200,
            priceBandLowerPaise: Math.round(Number(lowLakh) * 100000 * 100),
            priceBandUpperPaise: Math.round(Number(highLakh) * 100000 * 100),
            opensAt,
            closesAt,
            allotmentDueAt,
          },
        },
      });
      toast('Offer created in CONFIGURED.');
      onCreated(res.data.id);
    } catch (err) {
      setError(err as ApiError);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card>
      <h3 style={{ marginTop: 0 }}>Create offer (CONFIGURED)</h3>
      <form onSubmit={submit}>
        <div className="field">
          <label htmlFor="co-scheme">Scheme ID</label>
          <input id="co-scheme" className="input mono" value={schemeId} onChange={(e) => setSchemeId(e.target.value)} placeholder="uuid" />
        </div>
        <div className="grid2">
          <div className="field">
            <label htmlFor="co-type">Type</label>
            <select id="co-type" className="input" value={offerType} onChange={(e) => setOfferType(e.target.value)}>
              <option value="INITIAL">INITIAL</option>
              <option value="FOLLOW_ON">FOLLOW_ON</option>
            </select>
          </div>
          <div className="field">
            <label htmlFor="co-units">Units on offer</label>
            <input id="co-units" className="input num" value={units} onChange={(e) => setUnits(e.target.value)} inputMode="numeric" />
          </div>
        </div>
        <div className="grid2">
          <div className="field">
            <label htmlFor="co-min">Min bid units</label>
            <input id="co-min" className="input num" value={minBid} onChange={(e) => setMinBid(e.target.value)} inputMode="numeric" />
          </div>
          <div className="field">
            <label htmlFor="co-max">Max bid units</label>
            <input id="co-max" className="input num" value={maxBid} onChange={(e) => setMaxBid(e.target.value)} inputMode="numeric" />
          </div>
        </div>
        <div className="grid2">
          <div className="field">
            <label htmlFor="co-low">Price band low (₹ lakh)</label>
            <input id="co-low" className="input num" value={lowLakh} onChange={(e) => setLowLakh(e.target.value)} inputMode="decimal" />
          </div>
          <div className="field">
            <label htmlFor="co-high">Price band high (₹ lakh)</label>
            <input id="co-high" className="input num" value={highLakh} onChange={(e) => setHighLakh(e.target.value)} inputMode="decimal" />
          </div>
        </div>
        {error && <ErrorBox error={error} operator />}
        <button type="submit" className="btn primary" disabled={busy || !schemeId.trim()}>
          {busy ? 'Creating…' : 'Create offer'}
        </button>
      </form>
    </Card>
  );
}

export default function Admin() {
  const { offerId: routeOfferId } = useParams();
  const { mockRole, signedIn } = useSession();
  const [offerId, setOfferId] = useState(() => routeOfferId ?? localStorage.getItem(LS_OFFER) ?? '');
  const [draft, setDraft] = useState(() => routeOfferId ?? localStorage.getItem(LS_OFFER) ?? '');
  const offer = usePoll<Offer>(offerId ? `/offers/${offerId}` : null, {});
  const ballot = usePoll<BallotCeremony>(offerId ? `/offers/${offerId}/ballot` : null, {});

  if (!signedIn) {
    return (
      <>
        <h1>Operator console</h1>
        <Empty title="Sign in first" body="Operators act with a MANAGER, TRUSTEE or COMPLIANCE persona." action={<Link className="btn primary" to="/login">Sign in</Link>} />
      </>
    );
  }
  if (!mockRole) {
    return (
      <>
        <h1>Operator console</h1>
        <div className="alert warn" role="note">
          <p><strong>Awaiting persona.</strong> This console needs a MANAGER, TRUSTEE or COMPLIANCE persona — pick one on the sign-in screen. Separation of duties is legible here: trustee approval is a second party signing, never "you lack permission".</p>
        </div>
        <p><Link className="btn primary" to="/login">Choose persona →</Link></p>
      </>
    );
  }

  const load = (e: React.FormEvent) => {
    e.preventDefault();
    const v = draft.trim();
    setOfferId(v);
    localStorage.setItem(LS_OFFER, v);
  };

  const schemeId = (offer.data?.schemeId as string) ?? null;

  return (
    <>
      <h1>Operator console</h1>
      <p>
        Acting as <strong>{mockRole}</strong>. The console is driven by <span className="mono">readiness</span> —{' '}
        <span className="mono">nextExpected</span> + <span className="mono">canAdvance</span> + <span className="mono">blockers</span> — not by a row of buttons.
        Backend messages are shown verbatim; they explain the rule, not just the refusal.
      </p>

      <Card>
        <form onSubmit={load}>
          <div className="field" style={{ marginBottom: 8 }}>
            <label htmlFor="admin-offer">Offer ID</label>
            <input id="admin-offer" className="input mono" value={draft} onChange={(e) => setDraft(e.target.value)} placeholder="uuid of the offer to operate" autoComplete="off" />
            <p className="hint">Persisted locally. Deep-linkable as <span className="mono">/admin/:offerId</span> — copy the offer ID from the public offer page, or create a new offer below.</p>
          </div>
          <button type="submit" className="btn primary" disabled={!draft.trim()}>Load offer</button>
        </form>
      </Card>

      {!offerId && <CreateOffer onCreated={(id) => { setOfferId(id); setDraft(id); localStorage.setItem(LS_OFFER, id); }} />}

      {offerId && (
        <>
          {offer.loading && <LoadingCard label="Loading offer" />}
          {offer.error && <ErrorBox error={offer.error} operator />}
          {offer.data && (
            <Card flat>
              <p>
                <StatusPill kind="offer" status={offer.data.status} /> <Link to={`/offers/${offer.data.id}`}>Public offer view →</Link>
                {ballot.data?.resultRoot && <> · result <Hash value={ballot.data.resultRoot} /></>}
              </p>
            </Card>
          )}
          <ReadinessPanel offerId={offerId} />
          <CeremonyPanel offerId={offerId} />
          <SettlementPanel offerId={offerId} />
          <OutboxPanel schemeId={schemeId} />
          <Card>
            <h3 style={{ marginTop: 0 }}>Abort</h3>
            <p>Requires a reason — an abort with no recorded cause is indistinguishable from a mistake. Paused schemes (423) refuse everything except abort.</p>
            <AbortForm offerId={offerId} />
          </Card>
          <CreateOffer defaultSchemeId={schemeId ?? undefined} onCreated={(id) => { setOfferId(id); setDraft(id); localStorage.setItem(LS_OFFER, id); }} />
        </>
      )}
    </>
  );
}

function AbortForm({ offerId }: { offerId: string }) {
  const { token, mockRole } = useSession();
  const toast = useToast();
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!reason.trim()) return;
    setBusy(true);
    setError(null);
    try {
      await apiFetch(`/admin/offers/${offerId}/transitions`, {
        method: 'POST',
        token,
        mockRole: mockRole || undefined,
        idempotencyKey: newIdempotencyKey(),
        body: { to: 'ABORTED', reason: reason.trim() },
      });
      toast('Abort recorded.');
      window.dispatchEvent(new CustomEvent('acresync:mutated'));
    } catch (e) {
      setError(e as ApiError);
    } finally {
      setBusy(false);
    }
  };
  return (
    <form onSubmit={submit}>
      <div className="field">
        <label htmlFor="abort-reason">Reason (recorded)</label>
        <input id="abort-reason" className="input" value={reason} onChange={(e) => setReason(e.target.value)} placeholder="Why is this offer being aborted?" />
      </div>
      {error && <ErrorBox error={error} operator />}
      <button type="submit" className="btn danger" disabled={busy || !reason.trim()}>
        {busy ? 'Aborting…' : 'Abort offer'}
      </button>
    </form>
  );
}
