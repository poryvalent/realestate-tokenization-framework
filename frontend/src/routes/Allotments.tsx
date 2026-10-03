import { Link, useParams } from 'react-router-dom';
import { usePoll } from '../api/usePoll';
import { pageItems } from '../api/client';
import type { PublishedAllotment } from '../api/types';
import { int } from '../lib/format';
import { Card, LoadingCard, ErrorBox, StatusPill, Hash, Empty } from '../components/ui';

export default function Allotments() {
  const { id } = useParams();
  const { data, loading, error, refresh } = usePoll<PublishedAllotment[] | { items: PublishedAllotment[] }>(
    id ? `/offers/${id}/allotments?limit=200` : null, {},
  );
  const rows = pageItems(data);

  if (loading) return <LoadingCard lines={6} label="Loading allotments" />;
  if (error) {
    if (error.status === 404) {
      return (
        <>
          <p style={{ marginTop: 24 }}><Link to={`/offers/${id}`}>← Offer</Link></p>
          <h1>Allotments</h1>
          <div className="placeholder">
            <h3>Allotments publish after the draw</h3>
            <p>Every bid's outcome — winners and losers alike. Publishing only winners would make the draw unfalsifiable for exactly the people with the strongest reason to check it.</p>
          </div>
        </>
      );
    }
    return <ErrorBox error={error} retry={refresh} />;
  }

  const full = rows.filter((r) => r.outcome === 'FULL').length;
  const partial = rows.filter((r) => r.outcome === 'PARTIAL').length;

  return (
    <>
      <p style={{ marginTop: 24 }}><Link to={`/offers/${id}`}>← Offer</Link></p>
      <h1>Allotments</h1>
      <p>{int(rows.length)} published outcomes · {int(full)} full · {int(partial)} partial · the rest lost the draw or failed checks. Bidders appear only by leaf index and anchor — no PII, ever.</p>
      {rows.length === 0 && <Empty title="No allotments" />}
      {rows.length > 0 && (
        <Card flat>
          <div style={{ overflowX: 'auto' }}>
            <table className="ledger">
              <thead><tr><th className="r">Leaf</th><th>Anchor</th><th>Outcome</th><th className="r">Units</th></tr></thead>
              <tbody>
                {rows.map((r, i) => (
                  <tr key={r.leafIndex ?? i}>
                    <td className="r num">{int(r.leafIndex)}</td>
                    <td><Hash value={r.investorAnchor} /></td>
                    <td><StatusPill kind="bid" status={r.outcome ?? 'UNKNOWN'} /></td>
                    <td className="r num">{int(r.unitsAllotted)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </>
  );
}
