import { useEffect, useRef, type ReactNode } from 'react';

/**
 * Scroll + pointer parallax scene. Transform-only, rAF-driven, no react state.
 * - Children with `data-speed` drift vertically as the scene moves through the
 *   viewport (negative = lag behind, positive = rush ahead).
 * - Children with `data-depth` (px at full deflection) drift with the pointer,
 *   smoothed toward the target so motion settles instead of stopping dead.
 * Both may be combined on one layer. Skipped under prefers-reduced-motion.
 */
export function ParallaxScene({ children, className }: { children: ReactNode; className?: string }) {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const scene = ref.current;
    if (!scene) return;
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;

    const layers = Array.from(scene.querySelectorAll<HTMLElement>('[data-speed],[data-depth]'));
    if (layers.length === 0) return;

    let raf = 0;
    let tx = 0;
    let ty = 0;
    let cx = 0;
    let cy = 0;

    const frame = () => {
      raf = 0;
      const rect = scene.getBoundingClientRect();
      const off = rect.top + rect.height / 2 - window.innerHeight / 2;
      cx += (tx - cx) * 0.09;
      cy += (ty - cy) * 0.09;
      for (const el of layers) {
        const speed = Number(el.dataset.speed) || 0;
        const depth = Number(el.dataset.depth) || 0;
        const x = cx * depth;
        const y = off * speed + cy * depth;
        el.style.transform = `translate3d(${x.toFixed(1)}px, ${y.toFixed(1)}px, 0)`;
      }
      if (Math.abs(tx - cx) > 0.0005 || Math.abs(ty - cy) > 0.0005) {
        raf = requestAnimationFrame(frame);
      }
    };
    const requestFrame = () => {
      if (!raf) raf = requestAnimationFrame(frame);
    };

    const onScroll = () => requestFrame();
    const onPointer = (e: PointerEvent) => {
      const rect = scene.getBoundingClientRect();
      if (rect.width === 0) return;
      tx = (e.clientX - rect.left) / rect.width - 0.5;
      ty = (e.clientY - rect.top) / rect.height - 0.5;
      requestFrame();
    };
    const onLeave = () => {
      tx = 0;
      ty = 0;
      requestFrame();
    };

    requestFrame();
    window.addEventListener('scroll', onScroll, { passive: true });
    window.addEventListener('resize', onScroll);
    scene.addEventListener('pointermove', onPointer);
    scene.addEventListener('pointerleave', onLeave);
    return () => {
      if (raf) cancelAnimationFrame(raf);
      window.removeEventListener('scroll', onScroll);
      window.removeEventListener('resize', onScroll);
      scene.removeEventListener('pointermove', onPointer);
      scene.removeEventListener('pointerleave', onLeave);
    };
  }, []);

  return (
    <div ref={ref} className={className ? `px-scene ${className}` : 'px-scene'}>
      {children}
    </div>
  );
}
