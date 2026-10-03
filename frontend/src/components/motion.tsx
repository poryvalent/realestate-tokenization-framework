import { useEffect, useRef, useState, type ReactNode } from 'react';
import { motion, useInView, useMotionValue, useSpring, useTransform } from 'framer-motion';

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

/* Scroll-triggered reveal */
export function Io({ children, className, delay = 0, as }: { children: ReactNode; className?: string; delay?: number; as?: 'div' | 'section' | 'li' | 'span' }) {
  void as;
  return (
    <motion.div
      className={className}
      initial={{ opacity: 0, y: 24, filter: 'blur(4px)' }}
      whileInView={{ opacity: 1, y: 0, filter: 'blur(0px)' }}
      viewport={{ once: true, margin: '-60px' }}
      transition={{ duration: 0.55, delay: delay / 1000, ease: [0.25, 0.1, 0.25, 1] }}
    >
      {children}
    </motion.div>
  );
}

export function CountUp({ end, decimals = 0, prefix = '', suffix = '' }: { end: number; decimals?: number; prefix?: string; suffix?: string }) {
  const ref = useRef<HTMLSpanElement>(null);
  const inView = useInView(ref, { once: true, margin: '-40px' });
  const mv = useMotionValue(0);
  const spring = useSpring(mv, { duration: 1500, bounce: 0 });
  const [val, setVal] = useState(0);
  useEffect(() => {
    if (inView) mv.set(end);
  }, [inView, end, mv]);
  useEffect(() => spring.on('change', (v) => setVal(v)), [spring]);
  return (
    <span ref={ref} className="tabular-nums">
      {prefix}{val.toLocaleString('en-IN', { minimumFractionDigits: decimals, maximumFractionDigits: decimals })}{suffix}
    </span>
  );
}

export function Marquee({ items }: { items: string[] }) {
  const row = [...items, ...items];
  return (
    <div className="relative overflow-hidden border-y border-white/10 bg-white/[0.02] py-3" role="presentation">
      <motion.div
        className="flex w-max items-center gap-8 whitespace-nowrap text-sm text-white/50"
        animate={{ x: ['0%', '-50%'] }}
        transition={{ duration: 30, repeat: Infinity, ease: 'linear' }}
      >
        {row.map((t, i) => (
          <span key={i} className="flex items-center gap-8">
            {t} <span className="text-brand-blue/60">◆</span>
          </span>
        ))}
      </motion.div>
    </div>
  );
}

export function Magnetic({ children }: { children: ReactNode }) {
  return <span className="inline-block">{children}</span>;
}

export function SpotField({ children, className }: { children: ReactNode; className?: string }) {
  return <div className={className}>{children}</div>;
}

export function Scramble({ text, className }: { text: string; className?: string }) {
  return <span className={className}>{text}</span>;
}

export function Kinetic({ text, delay = 0 }: { text: string; delay?: number }) {
  const words = text.split(' ');
  return (
    <span aria-label={text}>
      {words.map((w, wi) => (
        <span key={wi} className="inline-block whitespace-pre">
          {w.split('').map((ch, ci) => (
            <motion.span
              key={ci}
              className="inline-block"
              initial={{ opacity: 0, y: 20, filter: 'blur(6px)' }}
              animate={{ opacity: 1, y: 0, filter: 'blur(0px)' }}
              transition={{ duration: 0.45, delay: delay / 1000 + (wi * 4 + ci) * 0.022, ease: [0.25, 0.1, 0.25, 1] }}
              aria-hidden="true"
            >
              {ch}
            </motion.span>
          ))}
          {wi < words.length - 1 ? ' ' : ''}
        </span>
      ))}
    </span>
  );
}

export function ScrollProgress() {
  const { scrollYProgress } = { scrollYProgress: useMotionValue(0) };
  void scrollYProgress;
  const [w, setW] = useState(0);
  useEffect(() => {
    const on = () => {
      const h = document.documentElement;
      const max = h.scrollHeight - h.clientHeight;
      setW(max > 0 ? h.scrollTop / max : 0);
    };
    on();
    window.addEventListener('scroll', on, { passive: true });
    return () => window.removeEventListener('scroll', on);
  }, []);
  return <div className="fixed inset-x-0 top-0 z-[80] h-[2px] origin-left bg-gradient-to-r from-brand-blue to-brand-mist" style={{ transform: `scaleX(${w})` }} aria-hidden="true" />;
}

export function Cursor() {
  return null;
}

export function Grain() {
  return <div className="noise" aria-hidden="true" />;
}

export function Preloader() {
  const [gone, setGone] = useState(false);
  useEffect(() => {
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
      setGone(true);
      return;
    }
    const t = setTimeout(() => setGone(true), 1100);
    return () => clearTimeout(t);
  }, []);
  if (gone) return null;
  return (
    <motion.div
      className="fixed inset-0 z-[100] flex flex-col items-center justify-center bg-brand-dark"
      exit={{ opacity: 0 }}
      animate={{ opacity: 1 }}
    >
      <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} className="text-2xl font-semibold tracking-tight text-white">
        AcreSync
      </motion.div>
      <motion.div
        className="mt-4 h-px w-40 bg-gradient-to-r from-transparent via-brand-blue to-transparent"
        animate={{ scaleX: [0, 1] }}
        transition={{ duration: 0.9 }}
      />
      <div className="mt-3 text-xs tracking-[0.25em] text-white/40">THE CHAIN ATTESTS · NEVER CUSTODIES</div>
    </motion.div>
  );
}

export function ParallaxHero({ children }: { children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  const mv = useMotionValue(0);
  const y = useTransform(mv, [0, 1], ['0%', '12%']);
  useEffect(() => {
    const on = () => {
      if (!ref.current) return;
      const r = ref.current.getBoundingClientRect();
      mv.set(Math.min(1, Math.max(0, -r.top / (r.height || 1))));
    };
    on();
    window.addEventListener('scroll', on, { passive: true });
    return () => window.removeEventListener('scroll', on);
  }, [mv]);
  return (
    <div ref={ref} className="relative overflow-hidden">
      <motion.div style={{ y }} className="absolute inset-0">
        {children}
      </motion.div>
    </div>
  );
}
