import { useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { usePoll } from '../api/usePoll';
import { apiFetch, ApiError } from '../api/client';
import type { Period, NdcfStatement, InclusionProof } from '../api/types';
import { inr, inrShort, calDate } from '../lib/format';
import { Card, LoadingCard, ErrorBox, Empty, StatusPill, Hash } from '../components/ui';

export default function PeriodView() {
  const { id } = useParams();
  const period = usePoll<Period>(id ? `/periods/${id}` : null, {});
  const ndcf = usePoll<NdcfStatement>(id ? `/periods/${id}/ndcf` : null, {});
  const [wallet, setWallet] = useState('');
  const [proof, setProof] = useState<InclusionProof | null>(null);
  const [proofLoading, setProofLoading] = useState(false);
  const [proofError, setProofError] = useState<ApiError | null>(null);

  const checkEntitlement = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!id || !wallet.trim()) return;
    setProofLoading(true);
    setProofError(null);
    try {
      const res = await apiFetch<InclusionProof>(`/periods/${id}/proofs/entitlements/${encodeURIComponent(wallet.trim())}`);
      setProof(res.data);
    } catch (err) {
      setProof(null);
      setProofError(err as ApiError);
    } finally {
      setProofLoading(false);
    }
  };

  if (period.loading) return <LoadingCard label="Loading period" />;
  if (period.error) return <ErrorBox error={period.error} retry={period.refresh} />;
  if (!period.data) return <Empty title="Period not found" />;
  const p = period.data;
  const n = ndcf.data;
  const ratio = n && n.distributionBps != null ? n.distributionBps / 100 : null;
  const floor = n && n.floorBps != null ? n.floorBps / 100 : 95;

  return (
    <>
      <p style={{ marginTop: 24 }}><Link to="/">← Schemes</Link></p>
      <h1>Distribution period</h1>
      <p><StatusPill kind="period" status={String(p.status ?? 'UNKNOWN')} /> Record date {calDate(p.recordDate)}</p>

      <h2>NDCF statement</h2>
      {ndcf.loading && <LoadingCard lines={3} label="Loading NDCF" />}
      {ndcf.error && (ndcf.error.status === 404 ? (
        <div className="placeholder">
          <h3>Statement publishes at period close</h3>
          <p>The distribution statement — gross rent, NDCF and the 95% test — appears here when the period closes. The floor applies to NDCF, not to gross rent.</p>
        </div>
      ) : <ErrorBox error={ndcf.error} retry={ndcf.refresh} />)}
      {n && (
        <Card>
          <dl className="kv">
            <dt>Gross</dt><dd className="num">{inr(n.grossPaise)}</dd>
            <dt>NDCF</dt><dd className="num">{inr(n.ndcfPaise)}</dd>
            <dt>Distributed</dt><dd className="num">{ratio != null ? `${ratio.toFixed(2)}%` : '—'} (floor {floor}%)</dd>
          </dl>
          {(n.lines ?? []).length > 0 && (
            <>
              <h3>Lines</h3>
              <table className="ledger">
                <thead><tr><th>Line</th><th>Direction</th><th className="r">Amount</th></tr></thead>
                <tbody>
                  {(n.lines ?? []).map((l, i) => (
                    <tr key={i}>
                      <td>{l.label ?? l.lineType ?? '—'}</td>
                      <td>{l.direction ?? '—'}</td>
                      <td className="r num">{inr(l.amountPaise)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              <p style={{ marginTop: 12 }}>Reference scheme, closed quarter: {inrShort(9500000)} distributed.</p>
            </>
          )}
        </Card>
      )}

      <h2>Entitlement proof</h2>
      <Card>
        <p>Public, no sign-in. Paste a wallet address to fetch its Merkle proof for this period.</p>
        <form onSubmit={checkEntitlement}>
          <div className="field">
            <label htmlFor="wallet">Wallet address</label>
            <input id="wallet" className="input mono" value={wallet} onChange={(e) => setWallet(e.target.value)} placeholder="0x…" autoComplete="off" />
          </div>
          <button type="submit" className="btn primary" disabled={proofLoading || !wallet.trim()}>
            {proofLoading ? 'Checking…' : 'Check entitlement'}
          </button>
        </form>
      </Card>
      {proofError && <ErrorBox error={proofError} />}
      {proof && (
        <Card>
          <dl className="kv">
            <dt>Leaf</dt><dd><Hash value={proof.leaf} /></dd>
            <dt>Root</dt><dd><Hash value={proof.root} /></dd>
          </dl>
        </Card>
      )}
    </>
  );
}
