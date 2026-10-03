import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from 'react';

interface Toast {
  id: number;
  text: string;
}

const Ctx = createContext<{ push: (text: string) => void } | null>(null);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const idRef = useRef(1);

  const push = useCallback((text: string) => {
    const id = idRef.current++;
    setToasts((t) => [...t, { id, text }]);
    // Toast close clock is the faster one; give readers time, then dismiss.
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), 4200);
  }, []);

  const value = useMemo(() => ({ push }), [push]);

  return (
    <Ctx.Provider value={value}>
      {children}
      <div
        aria-live="polite"
        style={{ position: 'fixed', bottom: 20, left: '50%', transform: 'translateX(-50%)', zIndex: 100, display: 'grid', gap: 8, width: 'min(480px, 92vw)' }}
      >
        {toasts.map((t) => (
          <ToastView key={t.id} text={t.text} onDone={() => setToasts((xs) => xs.filter((x) => x.id !== t.id))} />
        ))}
      </div>
    </Ctx.Provider>
  );
}

function ToastView({ text, onDone }: { text: string; onDone: () => void }) {
  const [open, setOpen] = useState(false);
  useState(() => {
    // mount open on next frame so the entrance transition plays
    requestAnimationFrame(() => requestAnimationFrame(() => setOpen(true)));
  });
  return (
    <div
      className={`t-toast${open ? ' is-open' : ''}`}
      role="status"
      onClick={() => {
        setOpen(false);
        setTimeout(onDone, 260);
      }}
      style={{
        background: 'var(--ink)', color: '#fff', borderRadius: 10,
        padding: '11px 16px', fontSize: '0.9rem', cursor: 'pointer', textAlign: 'center',
      }}
    >
      {text}
    </div>
  );
}

export function useToast(): (text: string) => void {
  const c = useContext(Ctx);
  if (!c) throw new Error('useToast must be used inside ToastProvider');
  return c.push;
}
