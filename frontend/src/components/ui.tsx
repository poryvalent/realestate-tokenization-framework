import { useEffect, useRef, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { AnimatePresence, motion } from 'framer-motion';
import { AlertTriangle, ArrowUpRight, ChevronDown, Clock } from 'lucide-react';
import { useState } from 'react';
import { statusTone, shortHex, offerStatusLabel } from '../lib/format';
import type { ApiError } from '../api/client';

const TONE: Record<string, string> = {
  ok: 'border-emerald-400/40 bg-emerald-500/10 text-emerald-300',
  warn: 'border-amber-400/40 bg-amber-500/10 text-amber-200',
  bad: 'border-rose-400/40 bg-rose-500/10 text-rose-300',
  info: 'border-blue-400/40 bg-blue-500/10 text-blue-200',
  neutral: 'border-white/20 bg-white/5 text-white/80',
};

export function StatusPill({ kind, status, label }: { kind: 'offer' | 'bid' | 'payout' | 'period' | 'outbox'; status: string; label?: string }) {
  const tone = statusTone(kind, status);
  return (
    <span className={`inline-flex items-center gap-1.5 rounded-none border px-2.5 py-0.5 text-xs font-mono uppercase tracking-wider ${TONE[tone]}`}>
      <span className="h-1.5 w-1.5 rounded-none bg-current" aria-hidden="true" />
      {label ?? (kind === 'offer' ? offerStatusLabel(status) : status.replace(/_/g, ' ').toLowerCase())}
    </span>
  );
}

export function Card({ children, flat }: { children: ReactNode; flat?: boolean }) {
  return (
    <motion.section
      initial={{ opacity: 0, y: 16 }}
      whileInView={{ opacity: 1, y: 0 }}
      viewport={{ once: true, margin: '-40px' }}
      transition={{ duration: 0.4, ease: [0.25, 0.1, 0.25, 1] }}
      className={`rounded-none border ${flat ? 'border-white/10 bg-[#050c1f]/60' : 'border-white/12 bg-[#07132e]/90 shadow-md'} p-5 sm:p-6`}
    >
      {children}
    </motion.section>
  );
}

export function ErrorBox({ error, retry, operator }: { error: ApiError; retry?: () => void; operator?: boolean }) {
  const waiting = error?.status === 409;
  return (
    <motion.div
      initial={{ opacity: 0, scale: 0.99 }}
      animate={{ opacity: 1, scale: 1 }}
      className={`rounded-none border p-5 ${waiting ? 'border-amber-400/30 bg-amber-500/10' : 'border-rose-400/30 bg-rose-500/10'}`}
      role="alert"
    >
      <p className="flex items-start gap-3 text-sm">
        {waiting ? <Clock className="mt-0.5 h-4 w-4 shrink-0 text-amber-200" /> : <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-rose-200" />}
        <span>
          <strong className="font-semibold text-white">
            {waiting ? 'Not yet — ' : ''}{(error?.code ?? error?.name ?? 'error').replace(/_/g, ' ')}
          </strong>
          <br />
          <span className="text-white/70">
            {operator || waiting ? error.message : <>Please wait a moment and try again. Reference <span className="font-mono">{error.requestId ?? 'n/a'}</span>.</>}
          </span>
        </span>
      </p>
      {retry && (
        <p className="mt-3">
          <button type="button" onClick={retry} className="rounded-none border border-white/30 bg-white/5 px-4 py-1.5 text-xs font-bold uppercase tracking-wider text-white transition hover:bg-white/15">
            Retry
          </button>
        </p>
      )}
    </motion.div>
  );
}

export function LoadingCard({ lines = 4, label = 'Loading' }: { lines?: number; label?: string }) {
  return (
    <div className="rounded-none border border-white/10 bg-[#07132e]/80 p-5 sm:p-6" role="status" aria-label={label}>
      <div className="space-y-3" aria-hidden="true">
        {Array.from({ length: lines }).map((_, i) => (
          <motion.div
            key={i}
            className="h-4 rounded-none bg-blue-900/30"
            style={{ width: `${92 - i * 9}%` }}
            animate={{ opacity: [0.4, 0.8, 0.4] }}
            transition={{ duration: 1.6, repeat: Infinity, delay: i * 0.12 }}
          />
        ))}
      </div>
    </div>
  );
}

export function Empty({ title, body, action }: { title: string; body?: string; action?: ReactNode }) {
  return (
    <div className="rounded-none border border-white/10 bg-[#07132e]/80 p-8 text-center">
      <h3 className="text-lg font-semibold text-white uppercase tracking-wider">{title}</h3>
      {body && <p className="mx-auto mt-2 max-w-md text-sm text-white/60">{body}</p>}
      {action && <div className="mt-5 flex justify-center">{action}</div>}
    </div>
  );
}

/** Reserved panel for content that publishes on the offer timetable. Never fabricates. */
export function Unbuilt({ title, body }: { title: string; body: string; endpoint: string }) {
  return (
    <div className="rounded-none border border-dashed border-white/20 bg-[#050c1f]/40 p-8 text-center">
      <p className="text-xs font-mono font-medium uppercase tracking-[0.2em] text-white/40">Publishes on the offer timetable</p>
      <h3 className="mt-2 text-lg font-semibold text-white uppercase tracking-wider">{title}</h3>
      <p className="mx-auto mt-2 max-w-lg text-sm text-white/60">{body}</p>
      <p className="mt-4">
        <Link to="/" className="text-xs font-bold uppercase tracking-wider text-white underline-offset-4 hover:underline">Back to schemes →</Link>
      </p>
    </div>
  );
}

export function Hash({ value, chars }: { value: string | null | undefined; chars?: number }) {
  if (!value) return <>—</>;
  return (
    <span className="font-mono text-[13px] text-white/80" title={value.toLowerCase()}>
      {shortHex(value, chars)}
    </span>
  );
}

export function EtherscanLink({ tx }: { tx: string | null | undefined }) {
  if (!tx) return <>—</>;
  return (
    <a
      className="inline-flex items-center gap-1 font-mono text-[13px] text-blue-300 underline-offset-4 hover:text-white hover:underline"
      href={`https://sepolia.etherscan.io/tx/${tx.toLowerCase()}`}
      target="_blank"
      rel="noreferrer"
      title={tx}
    >
      {shortHex(tx)} <ArrowUpRight className="h-3.5 w-3.5" />
    </a>
  );
}

export function Accordion({ title, children, defaultOpen }: { title: ReactNode; children: ReactNode; defaultOpen?: boolean }) {
  const [open, setOpen] = useState(!!defaultOpen);
  return (
    <div className="overflow-hidden rounded-none border border-white/10 bg-[#07132e]/60">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
        className="flex w-full items-center justify-between gap-3 px-5 py-3.5 text-left font-semibold text-white"
      >
        <span>{title}</span>
        <motion.span animate={{ rotate: open ? 180 : 0 }} transition={{ duration: 0.25 }}>
          <ChevronDown className="h-4 w-4 text-white/60" />
        </motion.span>
      </button>
      <AnimatePresence initial={false}>
        {open && (
          <motion.div
            initial={{ height: 0, opacity: 0 }}
            animate={{ height: 'auto', opacity: 1 }}
            exit={{ height: 0, opacity: 0 }}
            transition={{ duration: 0.3, ease: [0.25, 0.1, 0.25, 1] }}
          >
            <div className="border-t border-white/10 px-5 py-4 text-sm text-white/70">{children}</div>
          </motion.div>
        )}
      </AnimatePresence>
    </div>
  );
}

export function Reveal({ title, lede }: { title: ReactNode; lede?: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    ref.current?.classList.add('is-shown');
  }, []);
  return (
    <div ref={ref}>
      <motion.h1
        initial={{ opacity: 0, y: 20 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.5, ease: [0.25, 0.1, 0.25, 1] }}
        className="text-gradient text-4xl font-semibold leading-[1.05] tracking-tight sm:text-5xl lg:text-6xl"
      >
        {title}
      </motion.h1>
      {lede && (
        <motion.p
          initial={{ opacity: 0, y: 12 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ duration: 0.45, delay: 0.1 }}
          className="mt-3 max-w-2xl text-base text-white/60 sm:text-lg"
        >
          {lede}
        </motion.p>
      )}
    </div>
  );
}
