import { useState } from 'react';
import { Link, useParams, useSearchParams } from 'react-router-dom';
import { motion } from 'framer-motion';
import { ArrowLeft, Fingerprint, SearchCheck } from 'lucide-react';
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
    <div className="mx-auto max-w-2xl">
      <Link
        to={`/offers/${id}`}
        className="inline-flex items-center gap-1.5 text-sm text-brand-mist hover:underline"
      >
        <ArrowLeft className="h-3.5 w-3.5" /> Offer
      </Link>
      <motion.h1
        initial={{ opacity: 0, y: 24, filter: 'blur(6px)' }}
        animate={{ opacity: 1, y: 0, filter: 'blur(0px)' }}
        transition={{ duration: 0.6, ease: [0.25, 0.1, 0.25, 1] }}
        className="text-gradient mt-3 text-4xl font-semibold tracking-tight sm:text-5xl"
      >
        Bid proof checker
      </motion.h1>
      <motion.p
        initial={{ opacity: 0, y: 16 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.5, delay: 0.12 }}
        className="mt-4 text-sm leading-relaxed text-white/60"
      >
        Paste a bid reference. The response carries the leaf, the proof, the root, the anchor transaction,
        and the preimage fields — everything a third party needs to rebuild the leaf without guessing.{' '}
        <strong className="font-semibold text-white">Our own recomputation is a convenience, not evidence.</strong>
      </motion.p>

      <div className="mt-6">
        <Card>
          <form onSubmit={check} className="flex flex-col gap-3 sm:flex-row">
            <div className="grow">
              <label htmlFor="bidref" className="sr-only">Bid reference</label>
              <input
                id="bidref"
                className="w-full rounded-xl border border-white/15 bg-white/[0.04] px-4 py-2.5 font-mono text-sm text-white placeholder:text-white/30 outline-none transition focus:border-brand-blue/60 focus:ring-2 focus:ring-brand-blue/20"
                value={bidRef}
                onChange={(e) => setBidRef(e.target.value)}
                placeholder="e.g. BID-…"
                autoComplete="off"
              />
            </div>
            <button
              type="submit"
              disabled={loading || !bidRef.trim()}
              className="inline-flex items-center justify-center gap-2 rounded-full bg-white px-5 py-2.5 text-sm font-semibold text-[#020319] transition hover:bg-white/85 disabled:opacity-50"
            >
              <SearchCheck className="h-4 w-4" />
              {loading ? 'Checking…' : 'Check inclusion'}
            </button>
          </form>
        </Card>
      </div>

      {error && (
        <div className="mt-5">
          <ErrorBox error={error} />
        </div>
      )}
      {proof && (
        <motion.div
          initial={{ opacity: 0, y: 20 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ duration: 0.45 }}
          className="mt-5"
        >
          <Card>
            <h3 className="mt-0 flex items-center gap-2 text-base font-semibold text-white">
              <Fingerprint className="h-4 w-4 text-brand-mist" />
              Result {proof.verified ? '· our recomputation matches' : ''}
            </h3>
            <dl className="mt-4 grid grid-cols-[110px_1fr] gap-x-4 gap-y-2 text-sm">
              <dt className="text-white/40">Leaf</dt><dd><Hash value={proof.leaf} /></dd>
              <dt className="text-white/40">Root</dt><dd><Hash value={proof.root} /></dd>
              <dt className="text-white/40">Anchored tx</dt><dd><EtherscanLink tx={proof.anchoredTx} /></dd>
            </dl>
            <h3 className="mt-6 text-sm font-semibold uppercase tracking-[0.15em] text-white/50">Proof path</h3>
            <ol className="mt-2 space-y-1.5 font-mono text-[13px] text-white/75" style={{ overflowWrap: 'anywhere' }}>
              {(proof.proof ?? []).map((p, i) => (
                <li key={i} className="rounded-lg border border-white/10 bg-white/[0.03] px-3 py-1.5">
                  <span className="mr-2 text-white/35">{String(i).padStart(2, '0')}</span>{String(p)}
                </li>
              ))}
            </ol>
            {proof.preimage && (
              <>
                <h3 className="mt-6 text-sm font-semibold uppercase tracking-[0.15em] text-white/50">
                  Preimage (rebuild the leaf from this)
                </h3>
                <pre className="mt-2 overflow-x-auto rounded-xl border border-white/10 bg-black/40 p-4 font-mono text-xs leading-relaxed text-white/75">
                  {JSON.stringify(proof.preimage, null, 2)}
                </pre>
              </>
            )}
          </Card>
        </motion.div>
      )}
    </div>
  );
}
