import { useState } from 'react';
import { Link, useParams, useSearchParams } from 'react-router-dom';
import { apiFetch, ApiError } from '../api/client';
import type { InclusionProof } from '../api/types';
import { Card, ErrorBox, Hash, EtherscanLink } from '../components/ui';

export default function ProofBid() {
  const { id } = useParams();
  const [search] = useSearchParams();
  const [bidRef, setBidRef] = useState(() => search.get('ref') ?? '');
  const [proof, setProof] = useState<InclusionProof | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);

  const check = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!id || !bidRef.trim()) return;
    setLoading(true);
    setError(null);
    try {
      const res = await apiFetch<InclusionProof>(`/offers/${id}/proofs/bids/${encodeURIComponent(bidRef.trim())}`);
      setProof(res.data);
    } catch (err) {
      setProof(null);
      setError(err as ApiError);
    } finally {
      setLoading(false);
    }
  };

  return (
    <>
      <p style={{ marginTop: 24 }}><Link to={`/offers/${id}`}>← Offer</Link></p>
      <h1>Bid proof checker</h1>
      <p>Paste a bid reference. The response carries the leaf, the proof, the root, the anchor transaction, and the preimage fields — everything a third party needs to rebuild the leaf without guessing. <strong>Our own recomputation is a convenience, not evidence.</strong></p>

      <Card>
        <form onSubmit={check}>
          <div className="field">
            <label htmlFor="bidref">Bid reference</label>
            <input id="bidref" className="input mono" value={bidRef} onChange={(e) => setBidRef(e.target.value)} placeholder="e.g. BID-…" autoComplete="off" />
          </div>
          <button type="submit" className="btn primary" disabled={loading || !bidRef.trim()}>
            {loading ? 'Checking…' : 'Check inclusion'}
          </button>
        </form>
      </Card>

      {error && <ErrorBox error={error} />}
      {proof && (
        <Card>
          <h3 style={{ marginTop: 0 }}>Result {proof.verified ? '· our recomputation matches' : ''}</h3>
          <dl className="kv">
            <dt>Leaf</dt><dd><Hash value={proof.leaf} /></dd>
            <dt>Root</dt><dd><Hash value={proof.root} /></dd>
            <dt>Anchored tx</dt><dd><EtherscanLink tx={proof.anchoredTx} /></dd>
          </dl>
          <h3>Proof path</h3>
          <ol className="mono" style={{ fontSize: '0.82rem', overflowWrap: 'anywhere' }}>
            {(proof.proof ?? []).map((p, i) => <li key={i}>{String(p)}</li>)}
          </ol>
          {proof.preimage && (
            <>
              <h3>Preimage (rebuild the leaf from this)</h3>
              <pre className="mono" style={{ fontSize: '0.8rem', background: 'var(--surface-2)', padding: 12, borderRadius: 8, overflowX: 'auto' }}>
                {JSON.stringify(proof.preimage, null, 2)}
              </pre>
            </>
          )}
        </Card>
      )}
    </>
  );
}
