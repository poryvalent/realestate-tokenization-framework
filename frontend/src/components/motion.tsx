import { useEffect, useRef, useState, type CSSProperties, type ReactNode } from 'react';

export function useReducedMotion(): boolean {
  const [reduced, setReduced] = useState(
    () => typeof window !== 'undefined' && window.matchMedia('(prefers-reduced-motion: reduce)').matches,
  );
  useEffect(() => {
    const mq = window.matchMedia('(prefers-reduced-motion: reduce)');
    const on = () => setReduced(mq.matches);
    mq.addEventListener('change', on);
    return () => mq.removeEventListener('change', on);
  }, []);
  return reduced;
}

function useInViewOnce<T extends HTMLElement>(threshold = 0.25) {
  const ref = useRef<T>(null);
  const [inView, setInView] = useState(false);
  useEffect(() => {
    const el = ref.current;
    if (!el || inView) return;
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
      setInView(true);
      return;
    }
    const io = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) {
          setInView(true);
          io.disconnect();
        }
      },
      { threshold, rootMargin: '0px 0px -8% 0px' },
    );
    io.observe(el);
    return () => io.disconnect();
  }, [inView, threshold]);
  return { ref, inView };
}

/* Scroll-triggered reveal: fade + rise + unblur as the block enters. */
export function Io({
  children,
  className,
  delay = 0,
  as: Tag = 'div',
}: {
  children: ReactNode;
  className?: string;
  delay?: number;
  as?: 'div' | 'section' | 'li' | 'span';
}) {
  const { ref, inView } = useInViewOnce<HTMLDivElement>();
  return (
    <Tag
      ref={ref as never}
      className={className ? `io-reveal${inView ? ' is-in' : ''} ${className}` : `io-reveal${inView ? ' is-in' : ''}`}
      style={{ '--io-delay': `${delay}ms` } as CSSProperties}
    >
      {children}
    </Tag>
  );
}

/* Animated figure: counts 0 → end with an expo-out ease the first time seen. */
export function CountUp({
  end,
  decimals = 0,
  prefix = '',
  suffix = '',
}: {
  end: number;
  decimals?: number;
  prefix?: string;
  suffix?: string;
}) {
  const { ref, inView } = useInViewOnce<HTMLSpanElement>(0.5);
  const [val, setVal] = useState(0);
  useEffect(() => {
    if (!inView) return;
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
      setVal(end);
      return;
    }
    let raf = 0;
    const t0 = performance.now();
    const dur = 1500;
    const tick = (t: number) => {
      const p = Math.min(1, (t - t0) / dur);
      const eased = p === 1 ? 1 : 1 - Math.pow(2, -10 * p);
      setVal(end * eased);
      if (p < 1) raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, [inView, end]);
  return (
    <span ref={ref} className="num">
      {prefix}
      {val.toLocaleString('en-IN', { minimumFractionDigits: decimals, maximumFractionDigits: decimals })}
      {suffix}
    </span>
  );
}

/* Infinite headline ticker. Second copy is aria-hidden; still under reduced motion. */
export function Marquee({ items }: { items: string[] }) {
  const row = (hidden: boolean) => (
    <div className="mq-track" aria-hidden={hidden || undefined}>
      {items.map((t, i) => (
        <span key={i} className="mq-item">
          {t}
          <span className="mq-sep" aria-hidden="true">◆</span>
        </span>
      ))}
    </div>
  );
  return (
    <div className="mq" role="presentation">
      {row(false)}
      {row(true)}
    </div>
  );
}

/* Magnetic pull: the child drifts a few px toward the cursor. Fine pointers only. */
export function Magnetic({ children }: { children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;
    if (window.matchMedia('(hover: none)').matches) return;
    let raf = 0;
    const move = (e: PointerEvent) => {
      const r = el.getBoundingClientRect();
      const dx = e.clientX - (r.left + r.width / 2);
      const dy = e.clientY - (r.top + r.height / 2);
      const dist = Math.hypot(dx, dy);
      const range = Math.max(r.width, r.height) * 1.2;
      cancelAnimationFrame(raf);
      raf = requestAnimationFrame(() => {
        if (dist < range) {
          const pull = (1 - dist / range) * 7;
          el.style.transform = `translate3d(${(dx / (dist || 1)) * pull}px, ${(dy / (dist || 1)) * pull}px, 0)`;
        } else {
          el.style.transform = 'translate3d(0, 0, 0)';
        }
      });
    };
    const reset = () => {
      cancelAnimationFrame(raf);
      el.style.transform = 'translate3d(0, 0, 0)';
    };
    window.addEventListener('pointermove', move, { passive: true });
    window.addEventListener('pointerleave', reset);
    return () => {
      cancelAnimationFrame(raf);
      window.removeEventListener('pointermove', move);
      window.removeEventListener('pointerleave', reset);
    };
  }, []);
  return (
    <span ref={ref} className="mg" style={{ display: 'inline-block' }}>
      {children}
    </span>
  );
}

/* Spotlight + tilt field: cards glow toward the cursor and lean into it. */
export function SpotField({ children, className }: { children: ReactNode; className?: string }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const field = ref.current;
    if (!field) return;
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;
    if (window.matchMedia('(hover: none)').matches) return;
    const move = (e: PointerEvent) => {
      for (const card of field.querySelectorAll<HTMLElement>('.spot-card')) {
        const r = card.getBoundingClientRect();
        const px = (e.clientX - r.left) / r.width - 0.5;
        const py = (e.clientY - r.top) / r.height - 0.5;
        card.style.setProperty('--mx', `${e.clientX - r.left}px`);
        card.style.setProperty('--my', `${e.clientY - r.top}px`);
        card.style.transform = `perspective(900px) rotateX(${(-py * 5).toFixed(2)}deg) rotateY(${(px * 5).toFixed(2)}deg)`;
      }
    };
    const reset = () => {
      for (const card of field.querySelectorAll<HTMLElement>('.spot-card')) {
        card.style.transform = 'perspective(900px) rotateX(0deg) rotateY(0deg)';
      }
    };
    field.addEventListener('pointermove', move, { passive: true });
    field.addEventListener('pointerleave', reset);
    return () => {
      field.removeEventListener('pointermove', move);
      field.removeEventListener('pointerleave', reset);
    };
  }, []);
  return (
    <div ref={ref} className={className ?? 'spot-field'}>
      {children}
    </div>
  );
}

/* Decode-in: mono glyphs shuffle, then settle left to right. */
const SCRAMBLE_GLYPHS = 'ABCDEF0123456789<>/\\|#+*';
export function Scramble({ text, className }: { text: string; className?: string }) {
  const { ref, inView } = useInViewOnce<HTMLSpanElement>(0.5);
  const [out, setOut] = useState(text);
  useEffect(() => {
    if (!inView) return;
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;
    let raf = 0;
    const t0 = performance.now();
    const dur = 950;
    const tick = (t: number) => {
      const p = Math.min(1, (t - t0) / dur);
      const settled = Math.floor(p * text.length);
      let s = text.slice(0, settled);
      for (let i = settled; i < text.length; i++) {
        s += text[i] === ' ' ? ' ' : SCRAMBLE_GLYPHS[Math.floor(Math.random() * SCRAMBLE_GLYPHS.length)];
      }
      setOut(s);
      if (p < 1) raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, [inView, text]);
  return (
    <span ref={ref} className={className}>
      {out}
    </span>
  );
}

/* Kinetic headline: letters rise-blur in with a per-letter stagger. */
export function Kinetic({ text, delay = 0 }: { text: string; delay?: number }) {
  const [shown, setShown] = useState(false);
  useEffect(() => {
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
      setShown(true);
      return;
    }
    const id = requestAnimationFrame(() => requestAnimationFrame(() => setShown(true)));
    return () => cancelAnimationFrame(id);
  }, []);
  const words = text.split(' ');
  let li = 0;
  return (
    <span className={`kt${shown ? ' is-in' : ''}`} aria-label={text}>
      {words.map((w, wi) => (
        <span key={wi} aria-hidden="true" style={{ display: 'contents' }}>
          <span className="kt-word" aria-hidden="true">
            {w.split('').map((ch) => {
              const i = li++;
              return (
                <span key={i} className="kt-ch" style={{ '--kt-i': i, animationDelay: `${delay + i * 22}ms` } as CSSProperties}>
                  {ch}
                </span>
              );
            })}
          </span>
          {wi < words.length - 1 ? ' ' : ''}
        </span>
      ))}
    </span>
  );
}

/* Thin scroll-progress rule pinned to the viewport top. */
export function ScrollProgress() {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const bar = ref.current;
    if (!bar) return;
    let raf = 0;
    const update = () => {
      raf = 0;
      const h = document.documentElement;
      const max = h.scrollHeight - h.clientHeight;
      bar.style.transform = `scaleX(${max > 0 ? h.scrollTop / max : 0})`;
    };
    const onScroll = () => {
      if (!raf) raf = requestAnimationFrame(update);
    };
    update();
    window.addEventListener('scroll', onScroll, { passive: true });
    window.addEventListener('resize', onScroll);
    return () => {
      if (raf) cancelAnimationFrame(raf);
      window.removeEventListener('scroll', onScroll);
      window.removeEventListener('resize', onScroll);
    };
  }, []);
  return <div ref={ref} className="scroll-progress" aria-hidden="true" />;
}

/* Difference-blend cursor follower. Fine pointers only; native cursor stays. */
export function Cursor() {
  const dot = useRef<HTMLDivElement>(null);
  const ring = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const d = dot.current;
    const r = ring.current;
    if (!d || !r) return;
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;
    if (window.matchMedia('(hover: none), (pointer: coarse)').matches) return;
    d.style.opacity = '1';
    r.style.opacity = '1';
    let raf = 0;
    let x = -100;
    let y = -100;
    let rx = -100;
    let ry = -100;
    const frame = () => {
      raf = 0;
      rx += (x - rx) * 0.16;
      ry += (y - ry) * 0.16;
      d.style.transform = `translate3d(${x}px, ${y}px, 0) translate(-50%, -50%)`;
      r.style.transform = `translate3d(${rx}px, ${ry}px, 0) translate(-50%, -50%) scale(${r.dataset.hot === '1' ? 1.7 : 1})`;
      if (Math.abs(x - rx) > 0.1 || Math.abs(y - ry) > 0.1) raf = requestAnimationFrame(frame);
    };
    const wake = () => {
      if (!raf) raf = requestAnimationFrame(frame);
    };
    const move = (e: PointerEvent) => {
      x = e.clientX;
      y = e.clientY;
      wake();
    };
    const over = (e: MouseEvent) => {
      const t = e.target as HTMLElement | null;
      r.dataset.hot = t && t.closest('a, button, [role="tab"], select, input, .spot-card') ? '1' : '0';
    };
    window.addEventListener('pointermove', move, { passive: true });
    window.addEventListener('mouseover', over, { passive: true });
    return () => {
      if (raf) cancelAnimationFrame(raf);
      window.removeEventListener('pointermove', move);
      window.removeEventListener('mouseover', over);
    };
  }, []);
  return (
    <>
      <div ref={dot} className="cur-dot" aria-hidden="true" />
      <div ref={ring} className="cur-ring" aria-hidden="true" />
    </>
  );
}

export function Grain() {
  return <div className="grain" aria-hidden="true" />;
}

/* First-paint brand intro. Fades fast; skipped under reduced motion. */
export function Preloader() {
  const reduced = useReducedMotion();
  const [phase, setPhase] = useState<'in' | 'out' | 'gone'>(reduced ? 'gone' : 'in');
  useEffect(() => {
    if (reduced) return;
    const t1 = setTimeout(() => setPhase('out'), 1050);
    const t2 = setTimeout(() => setPhase('gone'), 1500);
    return () => {
      clearTimeout(t1);
      clearTimeout(t2);
    };
  }, [reduced]);
  if (phase === 'gone') return null;
  return (
    <div className={`loader${phase === 'out' ? ' is-out' : ''}`} aria-hidden="true">
      <div className="loader-mark">AcreSync</div>
      <div className="loader-rule" />
      <div className="loader-sub">The chain attests · never custodies</div>
    </div>
  );
}
