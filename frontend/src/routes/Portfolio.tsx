import { useState } from 'react';
import { Link } from 'react-router-dom';
import { useSession } from '../api/session';
import { usePoll } from '../api/usePoll';
import { pageItems } from '../api/client';
import type { Holding, Entitlement, Payout } from '../api/types';
import { inr, int } from '../lib/format';
import { Card, LoadingCard, ErrorBox, StatusPill, Empty } from '../components/ui';
import { Tabs } from '../components/Tabs';

type Tab = 'holdings' | 'income' | 'payouts';

export default function Portfolio() {
  const { token, signedIn } = useSession();
  const [tab, setTab] = useState<Tab>('holdings');
  const holdings = usePoll<Holding[] | { items: Holding[] }>(signedIn ? '/me/holdings' : null, { token });
  const entitlements = usePoll<Entitlement[] | { items: Entitlement[] }>(signedIn && tab === 'income' ? '/me/entitlements' : null, { token });
  const payouts = usePoll<Payout[] | { items: Payout[] }>(signedIn && tab === 'payouts' ? '/me/payouts' : null, { token });

  if (!signedIn) {
    return (
      <>
        <h1>Portfolio</h1>
        <Empty title="Not signed in" action={<Link className="btn primary" to="/login">Sign in</Link>} />
      </>
    );
  }

  const hList = pageItems(holdings.data);
  const eList = pageItems(entitlements.data);
  const pList = pageItems(payouts.data);

  return (
    <>
      <h1>Portfolio</h1>
      <p style={{ marginBottom: 16 }}>
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
      </p>

      {tab === 'holdings' && (
        <>
          {holdings.loading && <LoadingCard lines={3} label="Loading holdings" />}
          {holdings.error && <ErrorBox error={holdings.error} retry={holdings.refresh} />}
          {!holdings.loading && !holdings.error && hList.length === 0 && (
            <Empty title="No units yet" body="Holdings appear here once an offer you won settles." />
          )}
          {hList.map((h, i) => (
            <Card key={h.schemeId ?? i}>
              <h3 style={{ marginTop: 0 }}>{h.schemeName ?? `Scheme ${String(h.schemeId).slice(0, 8)}`}</h3>
              <p className="num" style={{ fontSize: '1.6rem', fontWeight: 700, color: 'var(--ink)', marginBottom: 0 }}>{int(h.units)} units</p>
            </Card>
          ))}
        </>
      )}

      {tab === 'income' && (
        <>
          {entitlements.loading && <LoadingCard lines={4} label="Loading entitlements" />}
          {entitlements.error && <ErrorBox error={entitlements.error} retry={entitlements.refresh} />}
          {!entitlements.loading && !entitlements.error && eList.length === 0 && (
            <Empty title="No entitlements" body="Income per period lands here after the first distribution." />
          )}
          {eList.map((e, i) => (
            <Card key={e.periodId ?? i}>
              <h3 style={{ marginTop: 0 }}>{e.schemeName ?? 'Entitlement'} · {int(e.units)} units</h3>
              <dl className="kv">
                <dt>Gross</dt><dd className="num">{inr(e.grossPaise)}</dd>
                <dt>Tax withheld</dt><dd className="num">{inr(e.taxWithheldPaise)}</dd>
                <dt>Net payable</dt><dd className="num">{inr(e.netPayablePaise)}</dd>
              </dl>
              {e.payable === false && (
                <div className="alert info"><p>Tiny amount, below the provider's minimum — it carries forward rather than being dropped.</p></div>
              )}
            </Card>
          ))}
        </>
      )}

      {tab === 'payouts' && (
        <>
          <div className="alert info" role="note">
            <p>Money moves over regulated bank rails — payouts are instructed to your verified account, never held by the platform. And <strong>SETTLED is not final</strong>: a bank can still reverse it, which is why REVERSED is a separate state.</p>
          </div>
          {payouts.loading && <LoadingCard lines={4} label="Loading payouts" />}
          {payouts.error && <ErrorBox error={payouts.error} retry={payouts.refresh} />}
          {!payouts.loading && !payouts.error && pList.length === 0 && (
            <Empty title="No payouts" body="Payout instructions and their settlement state will show here." />
          )}
          {pList.map((p, i) => (
            <Card key={p.id ?? i}>
              <p><StatusPill kind="payout" status={p.status ?? 'UNKNOWN'} /> {p.simulated && <StatusPill kind="payout" status="SIMULATED" label="Simulated" />}</p>
              <dl className="kv">
                <dt>Amount</dt><dd className="num">{inr(p.netPaise ?? p.amountPaise)}</dd>
                <dt>UTR</dt><dd className="mono">{p.utr ?? '—'}</dd>
              </dl>
            </Card>
          ))}
        </>
      )}
    </>
  );
}
