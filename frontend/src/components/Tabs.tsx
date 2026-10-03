import { useEffect, useLayoutEffect, useRef } from 'react';

/**
 * Segmented control with the sliding pill (t-tabs hooks, transitions.css owns motion).
 * First paint + resize write position with `transition: none` + reflow, per the recipe.
 */
export function Tabs<T extends string>({ options, value, onChange, label }: {
  options: { value: T; label: string }[];
  value: T;
  onChange: (v: T) => void;
  label: string;
}) {
  const barRef = useRef<HTMLDivElement>(null);
  const pillRef = useRef<HTMLSpanElement>(null);

  const moveTo = (animate: boolean) => {
    const bar = barRef.current;
    const pill = pillRef.current;
    if (!bar || !pill) return;
    const active = bar.querySelector<HTMLButtonElement>(`[data-val="${value}"]`);
    if (!active) return;
    if (!animate) {
      const prev = pill.style.transition;
      pill.style.transition = 'none';
      pill.style.transform = `translateX(${active.offsetLeft}px)`;
      pill.style.width = `${active.offsetWidth}px`;
      void pill.offsetWidth; // reflow
      pill.style.transition = prev;
    } else {
      pill.style.transform = `translateX(${active.offsetLeft}px)`;
      pill.style.width = `${active.offsetWidth}px`;
    }
  };

  useLayoutEffect(() => {
    moveTo(false);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [value]);
  useEffect(() => {
    moveTo(false);
    const onResize = () => moveTo(false);
    window.addEventListener('resize', onResize);
    return () => window.removeEventListener('resize', onResize);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div className="t-tabs" role="tablist" aria-label={label} ref={barRef}>
      <span className="t-tabs-pill" aria-hidden="true" ref={pillRef} />
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          role="tab"
          data-val={o.value}
          aria-selected={o.value === value ? 'true' : 'false'}
          className="t-tab"
          onClick={() => {
            onChange(o.value);
            requestAnimationFrame(() => moveTo(true));
          }}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}
