import { useEffect, useRef, useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { statusTone, shortHex, offerStatusLabel } from '../lib/format';
import type { ApiError } from '../api/client';

/* Status pill: the only coloured chip. Tone from statusTone(). */
export function StatusPill({ kind, status, label }: { kind: 'offer' | 'bid' | 'payout' | 'period' | 'outbox'; status: string; label?: string }) {
  const tone = statusTone(kind, status);
  return (
    <span className={`pill ${tone}`} data-status={status}>
      <span className="dot" aria-hidden="true" />
      {label ?? (kind === 'offer' ? offerStatusLabel(status) : status.replace(/_/g, ' ').toLowerCase())}
    </span>
  );
}

export function Card({ children, flat }: { children: ReactNode; flat?: boolean }) {
  return <section className={flat ? 'card flat' : 'card'}>{children}</section>;
}

export function ErrorBox({ error, retry, operator }: { error: ApiError; retry?: () => void; operator?: boolean }) {
  const waiting = error.status === 409;
  return (
    <div className={`alert ${waiting ? 'warn' : 'bad'}`} role="alert">
      <p>
        <strong>{waiting ? 'Not yet — ' : ''}{error.code.replace(/_/g, ' ')}</strong>
        {operator || waiting ? (
          <>
            <br />{error.message}
          </>
        ) : (
          <>
            <br />Please wait a moment and try again. If this persists, note reference{' '}
            <span className="mono">{error.requestId ?? 'n/a'}</span>.
          </>
        )}
      </p>
      {retry && (
        <p style={{ marginTop: 8 }}>
          <button type="button" className="btn small" onClick={retry}>Retry</button>
        </p>
      )}
    </div>
  );
}

export function LoadingCard({ lines = 4, label = 'Loading' }: { lines?: number; label?: string }) {
  return (
    <div className="card" role="status" aria-label={label}>
      <div className="skel-screen" aria-hidden="true">
        {Array.from({ length: lines }).map((_, i) => (
          <div className="skel-bar" key={i} style={{ width: `${92 - i * 9}%` }} />
        ))}
      </div>
    </div>
  );
}

export function Empty({ title, body, action }: { title: string; body?: string; action?: ReactNode }) {
  return (
    <div className="card">
      <div className="empty">
        <h3 style={{ marginTop: 0 }}>{title}</h3>
        {body && <p>{body}</p>}
        {action}
      </div>
    </div>
  );
}

/** Reserved panel for content that publishes on the offer timetable. Never fabricates. */
export function Unbuilt({ title, body }: { title: string; body: string; endpoint: string }) {
  return (
    <div className="placeholder">
      <h3>{title}</h3>
      <p>{body}</p>
      <p style={{ marginBottom: 0 }}>
        <Link to="/">Back to schemes</Link>
      </p>
    </div>
  );
}

export function Hash({ value, chars }: { value: string | null | undefined; chars?: number }) {
  if (!value) return <>—</>;
  return (
    <span className="mono hash" title={value.toLowerCase()}>
      {shortHex(value, chars)}
    </span>
  );
}

export function EtherscanLink({ tx }: { tx: string | null | undefined }) {
  if (!tx) return <>—</>;
  return (
    <a className="mono" href={`https://sepolia.etherscan.io/tx/${tx.toLowerCase()}`} target="_blank" rel="noreferrer" title={tx}>
      {shortHex(tx)} ↗
    </a>
  );
}

/** Accordion disclosure (t-acc hooks, transitions.css owns motion). */
export function Accordion({ title, children, defaultOpen }: { title: ReactNode; children: ReactNode; defaultOpen?: boolean }) {
  const [open, setOpen] = useState(!!defaultOpen);
  return (
    <div className="t-acc" data-open={String(open)}>
      <button
        type="button"
        className="t-acc-head"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
        style={{
          all: 'unset', display: 'flex', width: '100%', cursor: 'pointer',
          alignItems: 'center', justifyContent: 'space-between', gap: 12,
          padding: '10px 2px', fontWeight: 650, boxSizing: 'border-box',
        }}
      >
        <span>{title}</span>
        <span className="t-acc-chevron" aria-hidden="true">
          <svg viewBox="0 0 16 16" width="16" height="16" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
            <path d="M4 6.5L8 10.5L12 6.5" />
          </svg>
        </span>
      </button>
      <div className="t-acc-panel">
        <div className="t-acc-panel-inner">{children}</div>
      </div>
    </div>
  );
}

/** Hero/section heading with the staggered entrance (t-stagger hooks). */
export function Reveal({ title, lede }: { title: ReactNode; lede?: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    void el.offsetHeight; // reflow so the entrance plays
    el.classList.add('is-shown');
  }, []);
  return (
    <div className="hero t-stagger" ref={ref}>
      <h1 className="t-stagger-line t-stagger-line--1">{title}</h1>
      {lede && <p className="lede t-stagger-line t-stagger-line--2">{lede}</p>}
    </div>
  );
}
