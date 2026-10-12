import { useCallback, useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';

interface SplashScreenProps {
  onComplete: () => void;
}

/** Fade-out duration before the splash unmounts (matches `duration-700` plus a small buffer). */
const FADE_OUT_MS = 800;

/**
 * Hard upper bound before the splash is dismissed regardless of video state.
 * The bundled clip runs ~5s, so this only matters if playback stalls or the file never loads.
 */
const SAFETY_TIMEOUT_MS = 8000;

export default function SplashScreen({ onComplete }: SplashScreenProps) {
  const [fading, setFading] = useState(false);
  const videoRef = useRef<HTMLVideoElement>(null);
  const finishedRef = useRef(false);
  const completeTimerRef = useRef<number | null>(null);
  const onCompleteRef = useRef(onComplete);
  useEffect(() => {
    onCompleteRef.current = onComplete;
  }, [onComplete]);

  // Idempotent: the video ending, a click, ESC, the safety timer and a load error can all
  // call this, but the fade and `onComplete` must only ever run once.
  const finish = useCallback(() => {
    if (finishedRef.current) return;
    finishedRef.current = true;
    setFading(true);
    completeTimerRef.current = window.setTimeout(() => onCompleteRef.current(), FADE_OUT_MS);
  }, []);

  useEffect(() => {
    // The intro always starts from the top of the page, even on a reload that would otherwise
    // restore an old scroll position into the middle of the gallery.
    const previousRestoration = history.scrollRestoration;
    history.scrollRestoration = 'manual';
    window.scrollTo(0, 0);
    const onLoad = () => window.scrollTo(0, 0);
    window.addEventListener('load', onLoad);

    // If autoplay is blocked (e.g. iOS Low Power Mode) the play() promise rejects: skip straight through
    // instead of leaving the viewer on a frozen frame.
    const playAttempt = videoRef.current?.play();
    if (playAttempt) playAttempt.catch(() => finish());

    const safety = window.setTimeout(finish, SAFETY_TIMEOUT_MS);

    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') finish();
    };
    window.addEventListener('keydown', onKeyDown);

    // Hold the page still while the intro plays so nothing scrolls underneath it.
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';

    return () => {
      window.clearTimeout(safety);
      window.removeEventListener('keydown', onKeyDown);
      window.removeEventListener('load', onLoad);
      document.body.style.overflow = previousOverflow;
      history.scrollRestoration = previousRestoration;
      if (completeTimerRef.current !== null) window.clearTimeout(completeTimerRef.current);
    };
  }, [finish]);

  // Rendered into <body> so it sits above every piece of app chrome, whatever stacking
  // context the route content happens to live in.
  return createPortal(
    <div
      role="presentation"
      onClick={finish}
      className={`fixed inset-0 z-[9999] bg-ink flex items-center overflow-hidden transition-opacity duration-700 cursor-pointer ${
        fading ? 'opacity-0 pointer-events-none' : 'opacity-100'
      }`}
    >
      {/* The clip is rendered on pure black; `lighten` lets that black fall through to the
          page's navy so the video's rectangle never shows as a box. */}
      <video
        ref={videoRef}
        autoPlay
        muted
        playsInline
        preload="auto"
        onEnded={finish}
        className="absolute top-[20%] left-[30%] md:left-[55%] md:-translate-x-1/2 w-[320px] sm:w-[390px] md:w-[750px] aspect-video object-contain"
        style={{ mixBlendMode: 'lighten' }}
      >
        <source src="/videos/splash.mp4" type="video/mp4" />
        <source
          src="https://d3u0tzju9qaucj.cloudfront.net/fe1f1a5c-425f-4f81-b26b-c5ba3efab5ec/5c6c1751-084c-433c-bfd8-8a806cc9dddc.mp4"
          type="video/mp4"
          onError={finish}
        />
      </video>

      {/* Skip control (top-right) */}
      <button
        type="button"
        onClick={finish}
        aria-label="Skip intro"
        className="absolute top-6 right-6 font-ui text-[10px] tracking-[0.25em] uppercase text-white/40 hover:text-white transition-colors duration-200 border border-white/10 px-3 py-1.5 rounded-sm"
      >
        Skip [ESC]
      </button>

      {/* Status caption (bottom-left) */}
      <div className="absolute bottom-6 left-6 font-ui text-[11px] tracking-[0.25em] uppercase text-white/50">
        ACRESYNC — SM-REIT VERIFICATION CONSOLE
      </div>
    </div>,
    document.body,
  );
}
