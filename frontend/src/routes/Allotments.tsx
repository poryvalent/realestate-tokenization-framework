import { Link, useParams } from 'react-router-dom';
import { motion } from 'framer-motion';
import { ArrowLeft } from 'lucide-react';
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
        <div className="mx-auto max-w-3xl">
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
            Allotments
          </motion.h1>
          <div className="mt-6 rounded-2xl border border-dashed border-white/20 bg-white/[0.02] p-8 text-center">
            <h3 className="text-lg font-semibold text-white">Allotments publish after the draw</h3>
            <p className="mx-auto mt-2 max-w-lg text-sm leading-relaxed text-white/60">
              Every bid's outcome — winners and losers alike. Publishing only winners would make the draw
              unfalsifiable for exactly the people with the strongest reason to check it.
            </p>
          </div>
        </div>
      );
    }
    return <ErrorBox error={error} retry={refresh} />;
  }

  const full = rows.filter((r) => r.outcome === 'FULL').length;
  const partial = rows.filter((r) => r.outcome === 'PARTIAL').length;

  return (
    <div className="mx-auto max-w-3xl">
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
        Allotments
      </motion.h1>
      <motion.p
        initial={{ opacity: 0, y: 16 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.5, delay: 0.12 }}
        className="mt-4 text-sm leading-relaxed text-white/60"
      >
        <span className="tabular-nums text-white/90">{int(rows.length)}</span> published outcomes ·{' '}
        <span className="tabular-nums text-white/90">{int(full)}</span> full ·{' '}
        <span className="tabular-nums text-white/90">{int(partial)}</span> partial · the rest lost the draw
        or failed checks. Bidders appear only by leaf index and anchor — no PII, ever.
      </motion.p>

      {rows.length === 0 && (
        <div className="mt-6">
          <Empty title="No allotments" />
        </div>
      )}
      {rows.length > 0 && (
        <motion.div
          initial={{ opacity: 0, y: 20 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ duration: 0.45, delay: 0.15 }}
          className="mt-6"
        >
          <Card flat>
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-white/10 text-left text-xs uppercase tracking-[0.15em] text-white/40">
                    <th className="px-3 py-2 text-right font-medium">Leaf</th>
                    <th className="px-3 py-2 font-medium">Anchor</th>
                    <th className="px-3 py-2 font-medium">Outcome</th>
                    <th className="px-3 py-2 text-right font-medium">Units</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((r, i) => (
                    <tr key={r.leafIndex ?? i} className="border-b border-white/5 last:border-0">
                      <td className="px-3 py-2 text-right tabular-nums text-white/85">{int(r.leafIndex)}</td>
                      <td className="px-3 py-2"><Hash value={r.investorAnchor} /></td>
                      <td className="px-3 py-2"><StatusPill kind="bid" status={r.outcome ?? 'UNKNOWN'} /></td>
                      <td className="px-3 py-2 text-right tabular-nums text-white/85">{int(r.unitsAllotted)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </Card>
        </motion.div>
      )}
    </div>
  );
}
