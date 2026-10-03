import { useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { motion } from 'framer-motion';
import { ArrowLeft, BadgeIndianRupee, Fingerprint, SearchCheck } from 'lucide-react';
import { usePoll } from '../api/usePoll';
import { apiFetch, type ApiError } from '../api/client';
import type { Period, NdcfStatement, InclusionProof } from '../api/types';
import { inr, inrShort, calDate } from '../lib/format';
import { Card, LoadingCard, ErrorBox, Empty, StatusPill, Hash, Reveal } from '../components/ui';
import { Io } from '../components/motion';

const inputCls =
  'w-full rounded-xl border border-white/15 bg-white/5 px-4 py-2.5 font-mono text-sm text-white placeholder:text-white/30 outline-none transition focus:border-brand-blue/60 focus:bg-white/[0.07]';

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

  if (period.loading) {
    return (
      <>
        <BackLink />
        <Reveal title={<>Distribution period<span className="text-brand-blue">.</span></>} />
        <div className="mt-8">
          <LoadingCard label="Loading period" />
        </div>
      </>
    );
  }
  if (period.error) {
    return (
      <>
        <BackLink />
        <Reveal title={<>Distribution period<span className="text-brand-blue">.</span></>} />
        <div className="mt-8">
          <ErrorBox error={period.error} retry={period.refresh} />
        </div>
      </>
    );
  }
  if (!period.data) {
    return (
      <>
        <BackLink />
        <Reveal title={<>Distribution period<span className="text-brand-blue">.</span></>} />
        <div className="mt-8">
          <Empty title="Period not found" />
        </div>
      </>
    );
  }
  const p = period.data;
  const n = ndcf.data;
  const ratio = n && n.distributionBps != null ? n.distributionBps / 100 : null;
  const floor = n && n.floorBps != null ? n.floorBps / 100 : 95;

  return (
    <>
      <BackLink />
      <Reveal
        title={
          <>
            Distribution period<span className="text-brand-blue">.</span>
          </>
        }
        lede="Gross rent in, NDCF out — and at least 95% of NDCF goes to holders, every period, enforced in Solidity."
      />
      <motion.div
        initial={{ opacity: 0, y: 12 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.45, delay: 0.15 }}
        className="mt-6 flex flex-wrap items-center gap-3"
      >
        <StatusPill kind="period" status={String(p.status ?? 'UNKNOWN')} />
        <span className="text-sm text-white/60">
          Record date <span className="font-medium text-white">{calDate(p.recordDate)}</span>
        </span>
      </motion.div>

      <Io className="mt-10">
        <SectionHead icon={<BadgeIndianRupee className="h-4 w-4 text-brand-blue" />} title="NDCF statement" />
      </Io>
      <div className="mt-4">
        {ndcf.loading && <LoadingCard lines={3} label="Loading NDCF" />}
        {ndcf.error &&
          (ndcf.error.status === 404 ? (
            <div className="rounded-2xl border border-dashed border-white/20 bg-white/[0.02] p-8 text-center">
              <p className="text-xs font-medium uppercase tracking-[0.2em] text-white/40">
                Publishes on the distribution timetable
              </p>
              <h3 className="mt-2 text-lg font-semibold text-white">Statement publishes at period close</h3>
              <p className="mx-auto mt-2 max-w-lg text-sm text-white/60">
                The distribution statement — gross rent, NDCF and the 95% test — appears here when the period
                closes. The floor applies to NDCF, not to gross rent.
              </p>
            </div>
          ) : (
            <ErrorBox error={ndcf.error} retry={ndcf.refresh} />
          ))}
        {n && (
          <Io>
            <Card>
              <dl className="grid grid-cols-[minmax(140px,220px)_1fr] gap-x-4 gap-y-3">
                <dt className="text-sm text-white/40">Gross</dt>
                <dd className="font-semibold tabular-nums text-white">{inr(n.grossPaise)}</dd>
                <dt className="text-sm text-white/40">NDCF</dt>
                <dd className="font-semibold tabular-nums text-white">{inr(n.ndcfPaise)}</dd>
                <dt className="text-sm text-white/40">Distributed</dt>
                <dd className="font-semibold tabular-nums text-white">
                  {ratio != null ? `${ratio.toFixed(2)}%` : '—'}{' '}
                  <span className="font-normal text-white/50">(floor {floor}%)</span>
                </dd>
              </dl>
              {(n.lines ?? []).length > 0 && (
                <>
                  <h3 className="mb-2 mt-6 text-sm font-semibold uppercase tracking-[0.15em] text-white/50">Lines</h3>
                  <div className="overflow-x-auto">
                    <table className="w-full min-w-[480px] border-collapse text-sm">
                      <thead>
                        <tr className="border-b border-white/15 text-left text-xs uppercase tracking-wider text-white/40">
                          <th className="px-2.5 py-2 font-medium">Line</th>
                          <th className="px-2.5 py-2 font-medium">Direction</th>
                          <th className="px-2.5 py-2 text-right font-medium">Amount</th>
                        </tr>
                      </thead>
                      <tbody>
                        {(n.lines ?? []).map((l, i) => (
                          <tr key={i} className="border-b border-white/5 last:border-0">
                            <td className="px-2.5 py-2.5 text-white/85">{l.label ?? l.lineType ?? '—'}</td>
                            <td className="px-2.5 py-2.5 text-white/60">{l.direction ?? '—'}</td>
                            <td className="px-2.5 py-2.5 text-right tabular-nums text-white">{inr(l.amountPaise)}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                  <p className="mt-3 text-sm text-white/50">
                    Reference scheme, closed quarter: {inrShort(9500000)} distributed.
                  </p>
                </>
              )}
            </Card>
          </Io>
        )}
      </div>

      <Io className="mt-10">
        <SectionHead icon={<Fingerprint className="h-4 w-4 text-brand-blue" />} title="Entitlement proof" />
      </Io>
      <Io className="mt-4">
        <Card>
          <p className="mb-4 text-sm text-white/60">
            Public, no sign-in. Paste a wallet address to fetch its Merkle proof for this period.
          </p>
          <form onSubmit={checkEntitlement}>
            <div className="mb-4">
              <label htmlFor="wallet" className="mb-1.5 block text-sm font-semibold text-white">
                Wallet address
              </label>
              <input
                id="wallet"
                className={inputCls}
                value={wallet}
                onChange={(e) => setWallet(e.target.value)}
                placeholder="0x…"
                autoComplete="off"
              />
            </div>
            <button
              type="submit"
              className="rounded-full bg-white px-5 py-2.5 text-sm font-semibold text-brand-dark transition hover:bg-brand-mist disabled:opacity-50"
              disabled={proofLoading || !wallet.trim()}
            >
              {proofLoading ? 'Checking…' : 'Check entitlement'}
            </button>
          </form>
        </Card>
      </Io>
      {proofError && (
        <div className="mt-4">
          <ErrorBox error={proofError} />
        </div>
      )}
      {proof && (
        <Io className="mt-4">
          <Card>
            <p className="mb-3 flex items-center gap-2 text-sm font-semibold text-white">
              <SearchCheck className="h-4 w-4 text-brand-blue" /> Proof found
            </p>
            <dl className="grid grid-cols-[minmax(100px,140px)_1fr] gap-x-4 gap-y-3">
              <dt className="text-sm text-white/40">Leaf</dt>
              <dd>
                <Hash value={proof.leaf} />
              </dd>
              <dt className="text-sm text-white/40">Root</dt>
              <dd>
                <Hash value={proof.root} />
              </dd>
            </dl>
          </Card>
        </Io>
      )}
    </>
  );
}

function BackLink() {
  return (
    <p className="mt-6">
      <Link to="/" className="inline-flex items-center gap-2 text-sm text-white/60 transition hover:text-white">
        <ArrowLeft className="h-4 w-4" /> Schemes
      </Link>
    </p>
  );
}

function SectionHead({ icon, title }: { icon: React.ReactNode; title: string }) {
  return (
    <div className="flex items-center gap-2.5">
      {icon}
      <h2 className="text-xl font-semibold tracking-tight text-white">{title}</h2>
      <span className="h-px flex-1 bg-gradient-to-r from-white/15 to-transparent" aria-hidden="true" />
    </div>
  );
}
