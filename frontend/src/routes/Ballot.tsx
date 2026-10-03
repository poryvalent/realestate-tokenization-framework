import { Link, useParams } from 'react-router-dom';
import { usePoll } from '../api/usePoll';
import type { BallotCeremony } from '../api/types';
import { Card, LoadingCard, ErrorBox, Empty, Hash, Accordion } from '../components/ui';

function Step({ done, title, body }: { done: boolean; title: string; body?: React.ReactNode }) {
  return (
    <li style={{ listStyle: 'none', display: 'flex', gap: 12, padding: '10px 0', borderBottom: '1px solid var(--line)' }}>
      <span aria-hidden="true" style={{
        flexShrink: 0, width: 22, height: 22, borderRadius: '50%', marginTop: 2,
        background: done ? 'var(--accent)' : 'var(--surface-2)', color: done ? '#fff' : 'var(--ink-3)',
        display: 'grid', placeItems: 'center', fontSize: '0.8rem', fontWeight: 700,
      }}>{done ? '✓' : '·'}</span>
      <div>
        <strong>{title}</strong>
        {body && <div style={{ fontSize: '0.9rem', color: 'var(--ink-2)' }}>{body}</div>}
      </div>
    </li>
  );
}

export default function Ballot() {
  const { id } = useParams();
  const { data, loading, error, refresh } = usePoll<BallotCeremony>(id ? `/offers/${id}/ballot` : null, { intervalMs: 8000 });

  if (loading) return <LoadingCard label="Loading ballot ceremony" />;
  if (error) {
    if (error.status === 404) {
      return (
        <>
          <p style={{ marginTop: 24 }}><Link to={`/offers/${id}`}>← Offer</Link></p>
          <h1>Ballot ceremony</h1>
          <div className="placeholder">
            <h3>Ceremony publishes at book freeze</h3>
            <p>The bid-book root is anchored first, then the seed commitment, then the draw — each step appears here the moment it is on-chain. Nothing to verify yet means nothing has been committed yet.</p>
          </div>
        </>
      );
    }
    return <ErrorBox error={error} retry={refresh} />;
  }
  if (!data) return <Empty title="No ceremony" />;
  const c = data;

  return (
    <>
      <p style={{ marginTop: 24 }}><Link to={`/offers/${id}`}>← Offer</Link></p>
      <h1>Ballot ceremony</h1>
      <p>
        The bid-book root is anchored <em>before</em> a seed exists, and the seed mixes a future block hash
        nobody can predict at commit time. Before the reveal, the absence of the seed <em>is</em> the
        security property — after it, the draw must be reproducible by anyone.
      </p>
      <p>Attempt <strong className="num">{c.attempt}</strong> of <strong className="num">{c.maxAttempts}</strong>
        {c.escalated ? ' · escalated to trustee' : ''} · Stage <strong>{c.stage}</strong></p>

      <Card>
        <ul style={{ margin: 0, padding: 0 }}>
          <Step done={!!c.bidbookRoot} title="Book frozen and anchored" body={c.bidbookRoot ? <>Root <Hash value={c.bidbookRoot} /></> : 'Waiting for the freeze.'} />
          <Step done={!!c.seedCommitment} title="Seed committed" body={c.seedCommitment ? <>Commitment <Hash value={c.seedCommitment} /></> : 'No commitment yet — nothing to influence exists.'} />
          <Step done={c.targetBlock != null} title="Target block chosen by the contract" body={c.targetBlock != null ? <>Block <span className="num mono">#{c.targetBlock}</span></> : 'Chosen at commit time, never by the caller.'} />
          <Step done={!!c.targetBlockHash} title="Target block mined" body={c.targetBlockHash ? <>Hash <Hash value={c.targetBlockHash} /></> : `Reveal window: ${c.revealWindowBlocks ?? '—'} blocks.`} />
          <Step done={!!c.seedPlaintext} title="Seed revealed" body={c.seedPlaintext ? <>Plaintext <Hash value={c.seedPlaintext} /> · final seed <Hash value={c.finalSeed} /></> : 'Absent until revealed — that absence is the point.'} />
          <Step done={!!c.resultRoot} title="Draw run, result anchored" body={c.resultRoot ? <>Result root <Hash value={c.resultRoot} /> · <Link to={`/offers/${id}/allotments`}>allotments →</Link></> : 'Every bid will get a published outcome, losers included.'} />
        </ul>
      </Card>

      <h2>Anchored values</h2>
      <Card>
        <dl className="kv">
          <dt>Bid-book root</dt><dd><Hash value={c.bidbookRoot} /></dd>
          <dt>Book CID digest</dt><dd><Hash value={c.bidbookCidDigest} /></dd>
          <dt>Seed commitment</dt><dd><Hash value={c.seedCommitment} /></dd>
          <dt>Target block hash</dt><dd><Hash value={c.targetBlockHash} /></dd>
          <dt>Final seed</dt><dd><Hash value={c.finalSeed} /></dd>
          <dt>Result root</dt><dd><Hash value={c.resultRoot} /></dd>
        </dl>
        <Accordion title="How to verify this yourself">
          <p>Fetch the frozen book, hash each bid leaf with SHA-256, rebuild the Merkle root, and compare it to the root above — then check the root against the anchor transaction on Sepolia. One hash function, standard library only.</p>
        </Accordion>
      </Card>
    </>
  );
}
