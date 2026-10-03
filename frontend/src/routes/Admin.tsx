import { useEffect, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { motion } from 'framer-motion';
import {
  ArrowLeft,
  Ban,
  Boxes,
  CircleGauge,
  Dices,
  Inbox,
  OctagonX,
  Rocket,
  ScrollText,
} from 'lucide-react';
import { useSession } from '../api/session';
import { usePoll } from '../api/usePoll';
import { apiFetch, newIdempotencyKey, pageItems, type ApiError } from '../api/client';
import type { BallotCeremony, Offer, OutboxEntry, Readiness, SettlementState } from '../api/types';
import { int } from '../lib/format';
import { Card, LoadingCard, ErrorBox, StatusPill, Empty, Hash, EtherscanLink, Reveal } from '../components/ui';
import { useToast } from '../components/Toast';
import { Io } from '../components/motion';

const LS_OFFER = 'acresync.adminOffer';

const inputCls =
  'w-full rounded-xl border border-white/15 bg-white/5 px-4 py-2.5 text-sm text-white placeholder:text-white/30 outline-none transition focus:border-brand-blue/60 focus:bg-white/[0.07]';
const btnPrimary =
  'inline-flex items-center justify-center gap-2 rounded-full bg-white px-5 py-2.5 text-sm font-semibold text-brand-dark transition hover:bg-brand-mist disabled:opacity-50';
const btnGhost =
  'inline-flex items-center justify-center gap-2 rounded-full border border-white/20 px-4 py-1.5 text-sm font-semibold text-white transition hover:bg-white/10 disabled:opacity-50';
const btnDanger =
  'inline-flex items-center justify-center gap-2 rounded-full bg-rose-400 px-5 py-2.5 text-sm font-semibold text-brand-dark transition hover:bg-rose-300 disabled:opacity-50';

function useAdminAuth() {
  return useSession();
}

async function postAdmin<T>(path: string, token: string | null, mockRole: string, body?: unknown): Promise<T> {
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
      <button type="button" className={btnGhost} disabled={disabled || busy} onClick={run}>
        {busy ? 'Working…' : label}
      </button>
      {error && (
        <div className="mt-2">
          <ErrorBox error={error} operator />
        </div>
      )}
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
    <Io>
      <Card>
        <p className="mb-3 flex items-center gap-2 text-xs font-medium uppercase tracking-[0.2em] text-white/40">
          <CircleGauge className="h-4 w-4 text-brand-blue" /> Readiness — act on this, not on buttons
        </p>
        <dl className="grid grid-cols-[minmax(140px,220px)_1fr] gap-x-4 gap-y-3">
          <dt className="text-sm text-white/40">Current</dt>
          <dd>
            <StatusPill kind="offer" status={d.currentStatus} />
          </dd>
          <dt className="text-sm text-white/40">Next expected</dt>
          <dd>{d.nextExpected ? <StatusPill kind="offer" status={d.nextExpected} /> : '— (terminal)'}</dd>
          <dt className="text-sm text-white/40">Can advance</dt>
          <dd className="font-medium text-white">{d.canAdvance ? 'yes' : 'no'}</dd>
          {d.paused && (
            <>
              <dt className="text-sm text-white/40">Paused</dt>
              <dd className="text-amber-200">yes — everything except abort is refused (423)</dd>
            </>
          )}
        </dl>
        {(d.blockers ?? []).length > 0 && (
          <>
            <h3 className="mb-2 mt-5 text-sm font-semibold uppercase tracking-[0.15em] text-white/50">
              Blockers ({d.blockers.length})
            </h3>
            <ul className="grid list-disc gap-1.5 pl-5 text-sm text-white/75">
              {d.blockers.map((b, i) => (
                <li key={i}>{b}</li>
              ))}
            </ul>
          </>
        )}
        <div className="mt-4">
          {d.nextExpected ? (
            <ActionButton
              label={d.canAdvance ? `Advance to ${d.nextExpected}` : `Next: ${d.nextExpected} (blocked — see above)`}
              path={`/admin/offers/${offerId}/transitions`}
              body={{ to: d.nextExpected }}
              disabled={!d.canAdvance}
            />
          ) : (
            <p className="text-sm text-white/50">Terminal state — nothing further to advance.</p>
          )}
        </div>
        <p className="mt-3 text-xs text-white/40">
          409s (<span className="font-mono">anchor_not_confirmed</span>) are waiting states, not errors — confirmations
          land a few seconds after each anchor, then the step succeeds on retry.
        </p>
      </Card>
    </Io>
  );
}

function CeremonyPanel({ offerId }: { offerId: string }) {
  const c = usePoll<BallotCeremony>(`/offers/${offerId}/ballot`, { intervalMs: 8000 });
  return (
    <Io>
      <Card>
        <p className="mb-3 flex items-center gap-2 text-xs font-medium uppercase tracking-[0.2em] text-white/40">
          <Dices className="h-4 w-4 text-brand-blue" /> Ballot ceremony
        </p>
        {c.loading && <LoadingCard lines={2} label="Loading ceremony" />}
        {c.error && <ErrorBox error={c.error} retry={c.refresh} operator />}
        {c.data && (
          <p className="text-sm text-white/70">
            Attempt <strong className="tabular-nums text-white">{c.data.attempt}</strong> of{' '}
            <strong className="tabular-nums text-white">{c.data.maxAttempts}</strong>
            {c.data.escalated ? ' · escalated — awaiting trustee (a second party), not a permission issue' : ''} ·
            Stage <strong className="text-white">{c.data.stage}</strong>
          </p>
        )}
        <div className="mt-3 flex flex-wrap gap-2">
          <ActionButton label="Freeze book" path={`/admin/offers/${offerId}/book/freeze`} />
          <ActionButton label="Commit seed" path={`/admin/offers/${offerId}/ballot/commit`} />
          <ActionButton label="Reveal seed" path={`/admin/offers/${offerId}/ballot/reveal`} />
          <ActionButton label="Recommit (window lapsed only)" path={`/admin/offers/${offerId}/ballot/recommit`} />
          <ActionButton label="Draw ballot" path={`/admin/offers/${offerId}/ballot/draw`} />
        </div>
        <p className="mt-3 text-xs text-white/40">
          Ordering is enforced server-side (commit → reveal → draw). <span className="font-mono">recommit</span> only
          after the reveal window lapses, capped at <span className="font-mono">maxAttempts</span> — after that a
          trustee must escalate. Each step after the freeze returns{' '}
          <span className="font-mono">409 anchor_not_confirmed</span> for ~5s, then succeeds on retry.
        </p>
        <p className="mb-0 mt-2 text-sm">
          <Link to={`/offers/${offerId}/ballot`} className="text-brand-mist underline-offset-4 hover:underline">
            Public ceremony view →
          </Link>
        </p>
      </Card>
    </Io>
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
        <Io>
          <Card>
            <p className="mb-3 flex items-center gap-2 text-xs font-medium uppercase tracking-[0.2em] text-white/40">
              <Boxes className="h-4 w-4 text-brand-blue" /> Settlement
            </p>
            <p className="text-sm text-white/70">
              Not started.{' '}
              <ActionButton label="Begin settlement" path={`/admin/offers/${offerId}/settlement/begin`} />
            </p>
            <p className="mb-0 mt-3 text-xs text-white/40">
              Binds permanently to the anchored ballot result. Covers the public side only (475 of 500 for the
              reference scheme) — the manager's 25 are credited outside the cursor.
            </p>
          </Card>
        </Io>
      );
    }
    return <ErrorBox error={s.error} retry={s.refresh} operator />;
  }
  if (!s.data) return <Empty title="No settlement" />;
  const d = s.data;
  const pct = d.expectedHolders ? Math.min(100, (d.creditedHolders / d.expectedHolders) * 100) : 0;

  return (
    <Io>
      <Card>
        <p className="mb-3 flex items-center gap-2 text-xs font-medium uppercase tracking-[0.2em] text-white/40">
          <Boxes className="h-4 w-4 text-brand-blue" /> Settlement — {d.stage}
        </p>
        <dl className="grid grid-cols-[minmax(140px,220px)_1fr] gap-x-4 gap-y-3">
          <dt className="text-sm text-white/40">Progress</dt>
          <dd className="tabular-nums text-white">
            {int(d.creditedHolders)} / {int(d.expectedHolders)} holders · {int(d.creditedUnits)} /{' '}
            {int(d.expectedUnits)} units
          </dd>
          <dt className="text-sm text-white/40">Next step</dt>
          <dd className="font-mono text-sm text-white/80">{d.nextStep ?? 'waiting for confirmations'}</dd>
        </dl>
        <div
          className="mt-4 h-2.5 overflow-hidden rounded-full bg-white/10"
          role="progressbar"
          aria-valuenow={Math.round(pct)}
          aria-valuemin={0}
          aria-valuemax={100}
        >
          <motion.i
            className="block h-full w-full origin-left rounded-full bg-gradient-to-r from-brand-blue to-brand-mist"
            animate={{ scaleX: pct / 100 }}
            transition={{ duration: 0.5, ease: [0.25, 0.1, 0.25, 1] }}
          />
        </div>
        {(d.batches ?? []).length > 0 && (
          <div className="mt-4 overflow-x-auto">
            <table className="w-full min-w-[480px] border-collapse text-sm">
              <thead>
                <tr className="border-b border-white/15 text-xs uppercase tracking-wider text-white/40">
                  <th className="px-2.5 py-2 text-right font-medium">From</th>
                  <th className="px-2.5 py-2 text-right font-medium">To</th>
                  <th className="px-2.5 py-2 text-right font-medium">Holders</th>
                  <th className="px-2.5 py-2 text-left font-medium">Submitted</th>
                </tr>
              </thead>
              <tbody>
                {(d.batches ?? []).map((b, i) => (
                  <tr key={i} className="border-b border-white/5 text-white/80 last:border-0">
                    <td className="px-2.5 py-2.5 text-right tabular-nums">{int(b.cursorFrom)}</td>
                    <td className="px-2.5 py-2.5 text-right tabular-nums">{int(b.cursorTo)}</td>
                    <td className="px-2.5 py-2.5 text-right tabular-nums">{int(b.holderCount)}</td>
                    <td className="px-2.5 py-2.5">{b.submitted ? 'yes' : 'no'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {(d.finalisation?.failures ?? []).length > 0 && (
          <>
            <h3 className="mb-2 mt-5 text-sm font-semibold uppercase tracking-[0.15em] text-rose-200">
              Finalisation failures ({d.finalisation!.failures!.length}) — all of them, not just the first
            </h3>
            <ul className="grid list-disc gap-1.5 pl-5 text-sm text-white/75">
              {d.finalisation!.failures!.map((f, i) => (
                <li key={i}>{f}</li>
              ))}
            </ul>
          </>
        )}
        <div className="mt-4 flex flex-wrap gap-2">
          <ActionButton
            label="Begin settlement"
            path={`/admin/offers/${offerId}/settlement/begin`}
            disabled={d.stage !== 'NOT_STARTED'}
          />
          <div>
            <button
              type="button"
              className={btnGhost}
              disabled={busy || d.stage !== 'IN_PROGRESS'}
              onClick={submitBatch}
            >
              {busy ? 'Submitting…' : `Submit next batch (cursorFrom=${int(d.creditedHolders)})`}
            </button>
          </div>
          <ActionButton
            label="Finalise settlement"
            path={`/admin/offers/${offerId}/settlement/finalise`}
            disabled={d.stage !== 'IN_PROGRESS'}
          />
        </div>
        {error && (
          <div className="mt-2">
            <ErrorBox error={error} operator />
          </div>
        )}
        <p className="mb-0 mt-3 text-xs text-white/40">
          <span className="font-mono">cursorFrom</span> must equal holders already credited, exactly. A batch that ran
          out of gas is retryable verbatim; a batch that succeeded is rejected by both key and cursor.{' '}
          <span className="font-mono">nextStep: null</span> means wait for the queued call to confirm.
        </p>
      </Card>
    </Io>
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
    <Io>
      <Card>
        <p className="mb-3 flex items-center gap-2 text-xs font-medium uppercase tracking-[0.2em] text-white/40">
          <Inbox className="h-4 w-4 text-brand-blue" /> Chain outbox — queued is not sent
        </p>
        {o.loading && <LoadingCard lines={2} label="Loading outbox" />}
        {o.error && <ErrorBox error={o.error} retry={o.refresh} operator />}
        {!o.loading && !o.error && rows.length === 0 && (
          <p className="text-sm text-white/50">No queued chain calls for this scheme.</p>
        )}
        {rows.length > 0 && (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[520px] border-collapse text-sm">
              <thead>
                <tr className="border-b border-white/15 text-left text-xs uppercase tracking-wider text-white/40">
                  <th className="px-2.5 py-2 font-medium">Kind</th>
                  <th className="px-2.5 py-2 font-medium">Status</th>
                  <th className="px-2.5 py-2 text-right font-medium">Confirmations</th>
                  <th className="px-2.5 py-2 font-medium">Tx</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((e, i) => (
                  <tr key={e.id ?? i} className="border-b border-white/5 text-white/80 last:border-0">
                    <td className="px-2.5 py-2.5 font-mono text-[13px]">{e.kind ?? '—'}</td>
                    <td className="px-2.5 py-2.5">
                      <StatusPill kind="outbox" status={e.status ?? 'UNKNOWN'} />
                    </td>
                    <td className="px-2.5 py-2.5 text-right tabular-nums">
                      {int(e.confirmations)} / {int(e.requiredConfirmations)}
                    </td>
                    <td className="px-2.5 py-2.5">
                      <EtherscanLink tx={e.txHash} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        <p className="mb-0 mt-3 text-xs text-white/40">
          Chain calls confirm asynchronously — the queue below tracks every call to depth before the next step unlocks.
        </p>
      </Card>
    </Io>
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
    <Io>
      <Card>
        <p className="mb-4 flex items-center gap-2 text-xs font-medium uppercase tracking-[0.2em] text-white/40">
          <Rocket className="h-4 w-4 text-brand-blue" /> Create offer (CONFIGURED)
        </p>
        <form onSubmit={submit}>
          <div className="mb-4">
            <label htmlFor="co-scheme" className="mb-1.5 block text-sm font-semibold text-white">
              Scheme ID
            </label>
            <input
              id="co-scheme"
              className={`${inputCls} font-mono`}
              value={schemeId}
              onChange={(e) => setSchemeId(e.target.value)}
              placeholder="uuid"
            />
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="mb-4">
              <label htmlFor="co-type" className="mb-1.5 block text-sm font-semibold text-white">
                Type
              </label>
              <select
                id="co-type"
                className={`${inputCls} appearance-none`}
                value={offerType}
                onChange={(e) => setOfferType(e.target.value)}
              >
                <option value="INITIAL">INITIAL</option>
                <option value="FOLLOW_ON">FOLLOW_ON</option>
              </select>
            </div>
            <div className="mb-4">
              <label htmlFor="co-units" className="mb-1.5 block text-sm font-semibold text-white">
                Units on offer
              </label>
              <input
                id="co-units"
                className={`${inputCls} tabular-nums`}
                value={units}
                onChange={(e) => setUnits(e.target.value)}
                inputMode="numeric"
              />
            </div>
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="mb-4">
              <label htmlFor="co-min" className="mb-1.5 block text-sm font-semibold text-white">
                Min bid units
              </label>
              <input
                id="co-min"
                className={`${inputCls} tabular-nums`}
                value={minBid}
                onChange={(e) => setMinBid(e.target.value)}
                inputMode="numeric"
              />
            </div>
            <div className="mb-4">
              <label htmlFor="co-max" className="mb-1.5 block text-sm font-semibold text-white">
                Max bid units
              </label>
              <input
                id="co-max"
                className={`${inputCls} tabular-nums`}
                value={maxBid}
                onChange={(e) => setMaxBid(e.target.value)}
                inputMode="numeric"
              />
            </div>
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="mb-4">
              <label htmlFor="co-low" className="mb-1.5 block text-sm font-semibold text-white">
                Price band low (₹ lakh)
              </label>
              <input
                id="co-low"
                className={`${inputCls} tabular-nums`}
                value={lowLakh}
                onChange={(e) => setLowLakh(e.target.value)}
                inputMode="decimal"
              />
            </div>
            <div className="mb-4">
              <label htmlFor="co-high" className="mb-1.5 block text-sm font-semibold text-white">
                Price band high (₹ lakh)
              </label>
              <input
                id="co-high"
                className={`${inputCls} tabular-nums`}
                value={highLakh}
                onChange={(e) => setHighLakh(e.target.value)}
                inputMode="decimal"
              />
            </div>
          </div>
          {error && (
            <div className="mb-4">
              <ErrorBox error={error} operator />
            </div>
          )}
          <button type="submit" className={btnPrimary} disabled={busy || !schemeId.trim()}>
            {busy ? 'Creating…' : 'Create offer'}
          </button>
        </form>
      </Card>
    </Io>
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
        <Reveal
          title={
            <>
              Operator console<span className="text-brand-blue">.</span>
            </>
          }
          lede="Run the lifecycle: readiness, ceremony, settlement. Driven by state, not by buttons."
        />
        <div className="mt-8">
          <Empty
            title="Sign in first"
            body="Operators act with a MANAGER, TRUSTEE or COMPLIANCE persona."
            action={
              <Link to="/login" className={btnPrimary}>
                Sign in
              </Link>
            }
          />
        </div>
      </>
    );
  }
  if (!mockRole) {
    return (
      <>
        <Reveal
          title={
            <>
              Operator console<span className="text-brand-blue">.</span>
            </>
          }
        />
        <div
          className="mt-8 flex items-start gap-3 rounded-2xl border border-amber-300/30 bg-amber-300/10 p-5 text-sm text-amber-100"
          role="note"
        >
          <Ban className="mt-0.5 h-4 w-4 shrink-0" />
          <p className="mb-0">
            <strong className="font-semibold text-white">Awaiting persona.</strong> This console needs a MANAGER,
            TRUSTEE or COMPLIANCE persona — pick one on the sign-in screen. Separation of duties is legible here:
            trustee approval is a second party signing, never "you lack permission".
          </p>
        </div>
        <p className="mt-4">
          <Link to="/login" className={btnPrimary}>
            Choose persona →
          </Link>
        </p>
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
      <Reveal
        title={
          <>
            Operator console<span className="text-brand-blue">.</span>
          </>
        }
        lede={
          <>
            Acting as <strong className="font-semibold text-white">{mockRole}</strong>. The console is driven by{' '}
            <span className="font-mono text-brand-mist">readiness</span> —{' '}
            <span className="font-mono text-brand-mist">nextExpected</span> +{' '}
            <span className="font-mono text-brand-mist">canAdvance</span> +{' '}
            <span className="font-mono text-brand-mist">blockers</span> — not by a row of buttons. Backend messages are
            shown verbatim; they explain the rule, not just the refusal.
          </>
        }
      />

      <Io className="mt-8">
        <Card>
          <p className="mb-4 flex items-center gap-2 text-xs font-medium uppercase tracking-[0.2em] text-white/40">
            <ScrollText className="h-4 w-4 text-brand-blue" /> Offer under operation
          </p>
          <form onSubmit={load}>
            <div className="mb-3">
              <label htmlFor="admin-offer" className="mb-1.5 block text-sm font-semibold text-white">
                Offer ID
              </label>
              <input
                id="admin-offer"
                className={`${inputCls} font-mono`}
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                placeholder="uuid of the offer to operate"
                autoComplete="off"
              />
              <p className="mt-1.5 text-xs text-white/40">
                Persisted locally. Deep-linkable as <span className="font-mono">/admin/:offerId</span> — copy the offer
                ID from the public offer page, or create a new offer below.
              </p>
            </div>
            <button type="submit" className={btnPrimary} disabled={!draft.trim()}>
              Load offer
            </button>
          </form>
        </Card>
      </Io>

      {!offerId && (
        <div className="mt-4">
          <CreateOffer
            onCreated={(id) => {
              setOfferId(id);
              setDraft(id);
              localStorage.setItem(LS_OFFER, id);
            }}
          />
        </div>
      )}

      {offerId && (
        <div className="mt-4 grid gap-4">
          {offer.loading && <LoadingCard label="Loading offer" />}
          {offer.error && <ErrorBox error={offer.error} operator />}
          {offer.data && (
            <Io>
              <Card flat>
                <p className="mb-0 flex flex-wrap items-center gap-2 text-sm text-white/70">
                  <ArrowLeft className="hidden" aria-hidden="true" />
                  <StatusPill kind="offer" status={offer.data.status} />{' '}
                  <Link
                    to={`/offers/${offer.data.id}`}
                    className="text-brand-mist underline-offset-4 hover:underline"
                  >
                    Public offer view →
                  </Link>
                  {ballot.data?.resultRoot && (
                    <>
                      {' '}
                      · result <Hash value={ballot.data.resultRoot} />
                    </>
                  )}
                </p>
              </Card>
            </Io>
          )}
          <ReadinessPanel offerId={offerId} />
          <CeremonyPanel offerId={offerId} />
          <SettlementPanel offerId={offerId} />
          <OutboxPanel schemeId={schemeId} />
          <Io>
            <Card>
              <p className="mb-2 flex items-center gap-2 text-xs font-medium uppercase tracking-[0.2em] text-white/40">
                <OctagonX className="h-4 w-4 text-rose-300" /> Abort
              </p>
              <p className="text-sm text-white/60">
                Requires a reason — an abort with no recorded cause is indistinguishable from a mistake. Paused schemes
                (423) refuse everything except abort.
              </p>
              <AbortForm offerId={offerId} />
            </Card>
          </Io>
          <CreateOffer
            defaultSchemeId={schemeId ?? undefined}
            onCreated={(id) => {
              setOfferId(id);
              setDraft(id);
              localStorage.setItem(LS_OFFER, id);
            }}
          />
        </div>
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
    <form onSubmit={submit} className="mt-3">
      <div className="mb-3">
        <label htmlFor="abort-reason" className="mb-1.5 block text-sm font-semibold text-white">
          Reason (recorded)
        </label>
        <input
          id="abort-reason"
          className={inputCls}
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          placeholder="Why is this offer being aborted?"
        />
      </div>
      {error && (
        <div className="mb-3">
          <ErrorBox error={error} operator />
        </div>
      )}
      <button type="submit" className={btnDanger} disabled={busy || !reason.trim()}>
        {busy ? 'Aborting…' : 'Abort offer'}
      </button>
    </form>
  );
}
