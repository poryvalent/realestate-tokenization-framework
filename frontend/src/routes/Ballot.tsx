import { Link, useParams } from 'react-router-dom';
import { motion } from 'framer-motion';
import { ArrowLeft, Check } from 'lucide-react';
import { usePoll } from '../api/usePoll';
import type { BallotCeremony } from '../api/types';
import { Card, LoadingCard, ErrorBox, Empty, Hash, Accordion } from '../components/ui';

function Step({ done, title, body }: { done: boolean; title: string; body?: React.ReactNode }) {
  return (
    <li className="flex list-none gap-3 border-b border-white/10 py-3.5 last:border-0">
      <span
        aria-hidden="true"
        className={`mt-0.5 grid h-[22px] w-[22px] shrink-0 place-items-center rounded-full text-xs font-bold ${
          done ? 'bg-white text-[#020319]' : 'border border-white/20 bg-white/5 text-white/40'
        }`}
      >
        {done ? <Check className="h-3.5 w-3.5" /> : '·'}
      </span>
      <div>
        <strong className="text-sm font-semibold text-white">{title}</strong>
        {body && <div className="mt-1 text-sm text-white/60">{body}</div>}
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
            Ballot ceremony
          </motion.h1>
          <div className="mt-6 rounded-2xl border border-dashed border-white/20 bg-white/[0.02] p-8 text-center">
            <h3 className="text-lg font-semibold text-white">Ceremony publishes at book freeze</h3>
            <p className="mx-auto mt-2 max-w-lg text-sm leading-relaxed text-white/60">
              The bid-book root is anchored first, then the seed commitment, then the draw — each step
              appears here the moment it is on-chain. Nothing to verify yet means nothing has been committed yet.
            </p>
          </div>
        </div>
      );
    }
    return <ErrorBox error={error} retry={refresh} />;
  }
  if (!data) return <Empty title="No ceremony" />;
  const c = data;

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
        Ballot ceremony
      </motion.h1>
      <motion.div
        initial={{ opacity: 0, y: 16 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.5, delay: 0.12 }}
        className="mt-4 space-y-3 text-sm leading-relaxed text-white/60"
      >
        <p>
          The bid-book root is anchored <em className="text-white/85">before</em> a seed exists, and the seed
          mixes a future block hash nobody can predict at commit time. Before the reveal, the absence of the
          seed <em className="text-white/85">is</em> the security property — after it, the draw must be
          reproducible by anyone.
        </p>
        <p className="tabular-nums">
          Attempt <strong className="font-semibold text-white">{c.attempt}</strong> of{' '}
          <strong className="font-semibold text-white">{c.maxAttempts}</strong>
          {c.escalated ? ' · escalated to trustee' : ''} · Stage <strong className="font-mono text-white">{c.stage}</strong>
        </p>
      </motion.div>

      <motion.div
        initial={{ opacity: 0, y: 20 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.45, delay: 0.15 }}
        className="mt-6"
      >
        <Card>
          <ul className="m-0 p-0">
            <Step done={!!c.bidbookRoot} title="Book frozen and anchored" body={c.bidbookRoot ? <>Root <Hash value={c.bidbookRoot} /></> : 'Waiting for the freeze.'} />
            <Step done={!!c.seedCommitment} title="Seed committed" body={c.seedCommitment ? <>Commitment <Hash value={c.seedCommitment} /></> : 'No commitment yet — nothing to influence exists.'} />
            <Step done={c.targetBlock != null} title="Target block chosen by the contract" body={c.targetBlock != null ? <>Block <span className="font-mono tabular-nums text-white/85">#{c.targetBlock}</span></> : 'Chosen at commit time, never by the caller.'} />
            <Step done={!!c.targetBlockHash} title="Target block mined" body={c.targetBlockHash ? <>Hash <Hash value={c.targetBlockHash} /></> : `Reveal window: ${c.revealWindowBlocks ?? '—'} blocks.`} />
            <Step done={!!c.seedPlaintext} title="Seed revealed" body={c.seedPlaintext ? <>Plaintext <Hash value={c.seedPlaintext} /> · final seed <Hash value={c.finalSeed} /></> : 'Absent until revealed — that absence is the point.'} />
            <Step done={!!c.resultRoot} title="Draw run, result anchored" body={c.resultRoot ? <>Result root <Hash value={c.resultRoot} /> · <Link className="text-brand-mist hover:underline" to={`/offers/${id}/allotments`}>allotments →</Link></> : 'Every bid will get a published outcome, losers included.'} />
          </ul>
        </Card>
      </motion.div>

      <h2 className="mt-8 text-xl font-semibold tracking-tight text-white">Anchored values</h2>
      <div className="mt-4 space-y-4">
        <Card>
          <dl className="grid grid-cols-[150px_1fr] gap-x-4 gap-y-2 text-sm sm:grid-cols-[180px_1fr]">
            <dt className="text-white/40">Bid-book root</dt><dd><Hash value={c.bidbookRoot} /></dd>
            <dt className="text-white/40">Book CID digest</dt><dd><Hash value={c.bidbookCidDigest} /></dd>
            <dt className="text-white/40">Seed commitment</dt><dd><Hash value={c.seedCommitment} /></dd>
            <dt className="text-white/40">Target block hash</dt><dd><Hash value={c.targetBlockHash} /></dd>
            <dt className="text-white/40">Final seed</dt><dd><Hash value={c.finalSeed} /></dd>
            <dt className="text-white/40">Result root</dt><dd><Hash value={c.resultRoot} /></dd>
          </dl>
          <div className="mt-4">
            <Accordion title="How to verify this yourself">
              <p>Fetch the frozen book, hash each bid leaf with SHA-256, rebuild the Merkle root, and compare it to the root above — then check the root against the anchor transaction on Sepolia. One hash function, standard library only.</p>
            </Accordion>
          </div>
        </Card>
      </div>
    </div>
  );
}
