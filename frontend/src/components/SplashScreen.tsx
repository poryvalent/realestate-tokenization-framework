import { useState, useRef, useEffect } from 'react';

interface SplashScreenProps {
  onComplete: () => void;
}

export default function SplashScreen({ onComplete }: SplashScreenProps) {
  const [fading, setFading] = useState(false);
  const videoRef = useRef<HTMLVideoElement>(null);

  const handleFinish = () => {
    if (fading) return;
    setFading(true);
    setTimeout(() => {
      onComplete();
    }, 800);
  };

  useEffect(() => {
    // Fallback safety timer if video fails or stalls
    const timer = setTimeout(() => {
      handleFinish();
    }, 6000);
    return () => clearTimeout(timer);
  }, []);

  return (
    <div
      onClick={handleFinish}
      className={`fixed inset-0 z-[9999] bg-ink flex items-center justify-center transition-opacity duration-700 cursor-pointer ${
        fading ? 'opacity-0 pointer-events-none' : 'opacity-100'
      }`}
    >
      <video
        ref={videoRef}
        src="https://d3u0tzju9qaucj.cloudfront.net/fe1f1a5c-425f-4f81-b26b-c5ba3efab5ec/5c6c1751-084c-433c-bfd8-8a806cc9dddc.mp4"
        autoPlay
        muted
        playsInline
        onEnded={handleFinish}
        className="absolute top-[20%] left-[30%] -translate-x-1/2 md:left-[55%] w-[320px] sm:w-[390px] md:w-[750px] aspect-video object-contain"
      />

      {/* Skip button top-right */}
      <button
        type="button"
        onClick={(e) => {
          e.stopPropagation();
          handleFinish();
        }}
        className="absolute top-6 right-6 font-geist text-[10px] tracking-[0.25em] uppercase text-white/40 hover:text-white transition-colors duration-200 border border-white/10 px-3 py-1.5"
      >
        Skip [ESC]
      </button>

      {/* Status caption bottom-left */}
      <div className="absolute bottom-6 left-6 font-geist text-[11px] tracking-[0.25em] uppercase text-white/50">
        ACRESYNC — SM-REIT VERIFICATION CONSOLE
      </div>
    </div>
  );
}
