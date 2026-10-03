import { Fragment } from 'react';
import { Link, useParams } from 'react-router-dom';
import { usePoll } from '../api/usePoll';
import { pageItems } from '../api/client';
import type { Offer, Period, Scheme } from '../api/types';
import { inrShort, int } from '../lib/format';
import { Card, LoadingCard, ErrorBox, StatusPill, Empty } from '../components/ui';

export default function Scheme() {
  const { id } = useParams();
  const scheme = usePoll<Scheme>(id ? `/schemes/${id}` : null, {});
  const offers = usePoll<Offer[] | { items: Offer[] }>(id ? `/schemes/${id}/offers` : null, {});
  const periods = usePoll<Period[] | { items: Period[] }>(id ? `/schemes/${id}/periods` : null, {});
  const offerList = pageItems(offers.data);
  const periodList = pageItems(periods.data);

  if (scheme.loading) return <LoadingCard label="Loading scheme" />;
  if (scheme.error) return <ErrorBox error={scheme.error} retry={scheme.refresh} />;
  const s = scheme.data;
  if (!s) return <Empty title="Scheme not found" />;

  return (
    <>
      <p style={{ marginTop: 24 }}><Link to="/">← Schemes</Link></p>
      <h1>{String(s.name ?? s.id)}</h1>
      <Card>
        <dl className="kv">
          <dt>Target corpus</dt><dd className="num">{inrShort(s.targetCorpusPaise)}</dd>
          <dt>Units</dt><dd className="num">{int(s.totalUnits)} total · {int(s.managerUnits)} manager</dd>
          <dt>Manager</dt><dd>{String(s.investmentManager ?? '—')}</dd>
          <dt>Trustee</dt><dd>{String(s.trustee ?? '—')}</dd>
        </dl>
        {s.contracts && (
          <dl className="kv">
            {Object.entries(s.contracts).map(([k, v]) => (
              <Fragment key={k}>
                <dt className="mono">{k}</dt>
                <dd><a className="mono hash" href={`https://sepolia.etherscan.io/address/${String(v).toLowerCase()}`} target="_blank" rel="noreferrer">{String(v)}</a></dd>
              </Fragment>
            ))}
          </dl>
        )}
      </Card>

      <h2>Offers</h2>
      {offers.loading && <LoadingCard lines={2} label="Loading offers" />}
      {offers.error && <ErrorBox error={offers.error} retry={offers.refresh} />}
      {!offers.loading && !offers.error && offerList.length === 0 && <Empty title="No offers" body="This scheme has no offers yet." />}
      {offerList.map((o) => (
        <Card key={o.id}>
          <h3 style={{ marginTop: 0 }}><Link to={`/offers/${o.id}`}>{o.offerType ?? 'Offer'} · {inrShort(o.terms?.priceBandLowerPaise)}–{inrShort(o.terms?.priceBandUpperPaise)} per unit</Link></h3>
          <p><StatusPill kind="offer" status={o.status} /></p>
          <p style={{ marginBottom: 0 }}>
            {int(o.terms?.unitsOnOffer)} units · {int(o.subscription?.bidCount)} bids
            {o.subscription?.oversubscriptionNumerator != null && <> · subscribed {o.subscription.oversubscriptionNumerator}/{o.subscription.oversubscriptionDenominator ?? '—'}</>} ·{' '}
            <Link to={`/offers/${o.id}`}>Details →</Link>
          </p>
        </Card>
      ))}

      <h2>Distribution periods</h2>
      {periods.loading && <LoadingCard lines={2} label="Loading periods" />}
      {periods.error && <ErrorBox error={periods.error} retry={periods.refresh} />}
      {!periods.loading && !periods.error && periodList.length === 0 && <Empty title="No periods" body="No distribution periods yet." />}
      {periodList.map((p) => (
        <Card key={p.id}>
          <h3 style={{ marginTop: 0 }}><Link to={`/periods/${p.id}`}>Period {String(p.id).slice(0, 8)}</Link></h3>
          <p style={{ marginBottom: 0 }}><StatusPill kind="period" status={String(p.status ?? 'UNKNOWN')} /> Record date {String(p.recordDate ?? '—')}</p>
        </Card>
      ))}
    </>
  );
}
