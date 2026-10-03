import { useState } from 'react';
import { Link } from 'react-router-dom';
import { motion } from 'framer-motion';
import { AlertCircle, Landmark, Wallet } from 'lucide-react';
import { useSession } from '../api/session';
import { usePoll } from '../api/usePoll';
import { pageItems } from '../api/client';
import type { Holding, Entitlement, Payout } from '../api/types';
import { inr, int } from '../lib/format';
import { Card, LoadingCard, ErrorBox, StatusPill, Empty, Reveal } from '../components/ui';
import { Tabs } from '../components/Tabs';
import { Io } from '../components/motion';

type Tab = 'holdings' | 'income' | 'payouts';

export default function Portfolio() {
  const { token, signedIn } = useSession();
  const [tab, setTab] = useState<Tab>('holdings');
  const holdings = usePoll<Holding[] | { items: Holding[] }>(signedIn ? '/me/holdings' : null, { token });
  const entitlements = usePoll<Entitlement[] | { items: Entitlement[] }>(
    signedIn && tab === 'income' ? '/me/entitlements' : null,
    { token },
  );
  const payouts = usePoll<Payout[] | { items: Payout[] }>(signedIn && tab === 'payouts' ? '/me/payouts' : null, {
    token,
  });

  if (!signedIn) {
    return (
      <>
        <Reveal
          title={
            <>
              Portfolio<span className="text-brand-blue">.</span>
            </>
          }
          lede="Holdings, income and payouts — what you own, what it earned, and what reached your bank."
        />
        <div className="mt-8">
          <Empty
            title="Not signed in"
            action={
              <Link
                to="/login"
                className="rounded-full bg-white px-5 py-2.5 text-sm font-semibold text-brand-dark transition hover:bg-brand-mist"
              >
                Sign in
              </Link>
            }
          />
        </div>
      </>
    );
  }

  const hList = pageItems(holdings.data);
  const eList = pageItems(entitlements.data);
  const pList = pageItems(payouts.data);

  return (
    <>
      <Reveal
        title={
          <>
            Portfolio<span className="text-brand-blue">.</span>
          </>
        }
        lede="Holdings, income and payouts — what you own, what it earned, and what reached your bank."
      />
      <motion.div
        initial={{ opacity: 0, y: 12 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.45, delay: 0.15 }}
        className="mb-6 mt-6"
      >
        <Tabs<Tab>
          label="Portfolio sections"
          value={tab}
          onChange={setTab}
          options={[
            { value: 'holdings', label: 'Holdings' },
            { value: 'income', label: 'Income' },
            { value: 'payouts', label: 'Payouts' },
          ]}
        />
      </motion.div>

      {tab === 'holdings' && (
        <>
          {holdings.loading && <LoadingCard lines={3} label="Loading holdings" />}
          {holdings.error && <ErrorBox error={holdings.error} retry={holdings.refresh} />}
          {!holdings.loading && !holdings.error && hList.length === 0 && (
            <Empty title="No units yet" body="Holdings appear here once an offer you won settles." />
          )}
          <div className="grid gap-4 sm:grid-cols-2">
            {hList.map((h, i) => (
              <Io key={h.schemeId ?? i} delay={Math.min(i, 5) * 60}>
                <Card>
                  <p className="mb-1 flex items-center gap-2 text-xs font-medium uppercase tracking-[0.2em] text-white/40">
                    <Wallet className="h-3.5 w-3.5 text-brand-blue" /> Holding
                  </p>
                  <h3 className="mt-0 text-lg font-semibold text-white">
                    {h.schemeName ?? `Scheme ${String(h.schemeId).slice(0, 8)}`}
                  </h3>
                  <p className="mb-0 mt-2 text-3xl font-semibold tabular-nums text-white">{int(h.units)} units</p>
                </Card>
              </Io>
            ))}
          </div>
        </>
      )}

      {tab === 'income' && (
        <>
          {entitlements.loading && <LoadingCard lines={4} label="Loading entitlements" />}
          {entitlements.error && <ErrorBox error={entitlements.error} retry={entitlements.refresh} />}
          {!entitlements.loading && !entitlements.error && eList.length === 0 && (
            <Empty title="No entitlements" body="Income per period lands here after the first distribution." />
          )}
          <div className="grid gap-4">
            {eList.map((e, i) => (
              <Io key={e.periodId ?? i} delay={Math.min(i, 5) * 60}>
                <Card>
                  <h3 className="mt-0 text-lg font-semibold text-white">
                    {e.schemeName ?? 'Entitlement'} · {int(e.units)} units
                  </h3>
                  <dl className="mt-3 grid grid-cols-[minmax(140px,220px)_1fr] gap-x-4 gap-y-2.5">
                    <dt className="text-sm text-white/40">Gross</dt>
                    <dd className="font-medium tabular-nums text-white">{inr(e.grossPaise)}</dd>
                    <dt className="text-sm text-white/40">Tax withheld</dt>
                    <dd className="font-medium tabular-nums text-white">{inr(e.taxWithheldPaise)}</dd>
                    <dt className="text-sm text-white/40">Net payable</dt>
                    <dd className="font-semibold tabular-nums text-white">{inr(e.netPayablePaise)}</dd>
                  </dl>
                  {e.payable === false && (
                    <div
                      className="mt-4 flex items-start gap-3 rounded-xl border border-sky-300/30 bg-sky-300/10 p-4 text-sm text-sky-100"
                      role="note"
                    >
                      <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" />
                      <p className="mb-0">
                        Tiny amount, below the provider's minimum — it carries forward rather than being dropped.
                      </p>
                    </div>
                  )}
                </Card>
              </Io>
            ))}
          </div>
        </>
      )}

      {tab === 'payouts' && (
        <>
          <div
            className="mb-4 flex items-start gap-3 rounded-2xl border border-white/10 bg-white/[0.03] p-5 text-sm text-white/70"
            role="note"
          >
            <Landmark className="mt-0.5 h-4 w-4 shrink-0 text-brand-blue" />
            <p className="mb-0">
              Money moves over regulated bank rails — payouts are instructed to your verified account, never held by
              the platform. And <strong className="font-semibold text-white">SETTLED is not final</strong>: a bank can
              still reverse it, which is why REVERSED is a separate state.
            </p>
          </div>
          {payouts.loading && <LoadingCard lines={4} label="Loading payouts" />}
          {payouts.error && <ErrorBox error={payouts.error} retry={payouts.refresh} />}
          {!payouts.loading && !payouts.error && pList.length === 0 && (
            <Empty title="No payouts" body="Payout instructions and their settlement state will show here." />
          )}
          <div className="grid gap-4">
            {pList.map((p, i) => (
              <Io key={p.id ?? i} delay={Math.min(i, 5) * 60}>
                <Card>
                  <p className="mb-3 flex flex-wrap items-center gap-2">
                    <StatusPill kind="payout" status={p.status ?? 'UNKNOWN'} />{' '}
                    {p.simulated && <StatusPill kind="payout" status="SIMULATED" label="Simulated" />}
                  </p>
                  <dl className="grid grid-cols-[minmax(140px,220px)_1fr] gap-x-4 gap-y-2.5">
                    <dt className="text-sm text-white/40">Amount</dt>
                    <dd className="font-semibold tabular-nums text-white">{inr(p.netPaise ?? p.amountPaise)}</dd>
                    <dt className="text-sm text-white/40">UTR</dt>
                    <dd className="font-mono text-sm text-white/80">{p.utr ?? '—'}</dd>
                  </dl>
                </Card>
              </Io>
            ))}
          </div>
        </>
      )}
    </>
  );
}
