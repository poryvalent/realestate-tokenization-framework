import { useEffect, useRef, type ReactNode } from 'react';

/** Centered dialog. Open scales up from --modal-scale; close dips to --modal-scale-close. */
export function Modal({ open, onClose, title, children }: { open: boolean; title: string; children: ReactNode; onClose: () => void }) {
  const boxRef = useRef<HTMLDivElement>(null);
  const closeMs = useRef(150);

  useEffect(() => {
    const v = getComputedStyle(document.documentElement).getPropertyValue('--modal-close-dur');
    const n = parseFloat(v);
    if (Number.isFinite(n)) closeMs.current = n;
  }, []);

  useEffect(() => {
    const box = boxRef.current;
    if (!box) return;
    if (open) {
      box.classList.remove('is-closing');
      // next frame so the open transition plays from the resting scale
      requestAnimationFrame(() => requestAnimationFrame(() => box.classList.add('is-open')));
      const onKey = (e: KeyboardEvent) => {
        if (e.key === 'Escape') onClose();
      };
      document.addEventListener('keydown', onKey);
      return () => document.removeEventListener('keydown', onKey);
    }
    box.classList.remove('is-open');
    box.classList.add('is-closing');
    const t = setTimeout(() => box.classList.remove('is-closing'), closeMs.current);
    return () => clearTimeout(t);
  }, [open, onClose]);

  if (!open) return null;

  return (
    <div
      role="presentation"
      onClick={onClose}
      style={{ position: 'fixed', inset: 0, background: 'rgba(22,24,29,0.45)', zIndex: 90, display: 'grid', placeItems: 'center', padding: 20 }}
    >
      <div
        ref={boxRef}
        className="t-modal"
        role="dialog"
        aria-modal="true"
        aria-label={title}
        onClick={(e) => e.stopPropagation()}
        style={{ background: 'var(--surface)', borderRadius: 12, maxWidth: 560, width: '100%', padding: 24, boxShadow: 'var(--shadow)' }}
      >
        <h2 style={{ marginTop: 0 }}>{title}</h2>
        {children}
      </div>
    </div>
  );
}
