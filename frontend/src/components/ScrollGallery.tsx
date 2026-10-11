import type { FC } from 'react';

interface ScrollGalleryProps {
  progress: number; // 0 to 1
  visible?: boolean;
}

const IMAGES = [
  {
    url: 'https://images.unsplash.com/photo-1486406146926-c627a92ad1ab?auto=format&fit=crop&w=2400&q=85',
    alt: 'Commercial Office SPV 01',
    label: 'COMMERCIAL OFFICE · SPV 01',
  },
  {
    url: 'https://images.unsplash.com/photo-1541888946425-d0fbb1861593?auto=format&fit=crop&w=2000&q=80',
    alt: 'Cyber Park SPV 02',
    label: 'CYBER PARK · SPV 02',
  },
  {
    url: 'https://images.unsplash.com/photo-1507679799987-c73779587ccf?auto=format&fit=crop&w=1800&q=80',
    alt: 'Logistics Hub SPV 03',
    label: 'LOGISTICS HUB · SPV 03',
  },
];

export const ScrollGallery: FC<ScrollGalleryProps> = ({ progress, visible = true }) => {
  if (!visible) return null;

  // Phase 1: 0.00 - 0.06 (Line 0 to 50px)
  const isPhase1 = progress >= 0 && progress < 0.06;
  const p1 = Math.min(1, Math.max(0, progress / 0.06));
  const line1Width = p1 * 50;

  // Phase 2: 0.06 - 0.16 (Line expands 50px to 100vw)
  const p2 = Math.min(1, Math.max(0, (progress - 0.06) / 0.10));
  // Ease-in-out cubic
  const p2Eased = p2 < 0.5 ? 4 * p2 * p2 * p2 : 1 - Math.pow(-2 * p2 + 2, 3) / 2;

  // Phase 3: 0.16 - 0.34 (Image 1)
  const showImg1 = progress >= 0.16;
  const p3 = Math.min(1, Math.max(0, (progress - 0.16) / 0.18));
  const img1Scale = 0.2 + p3 * 0.8;
  const img1Opacity = Math.min(1, p3 * 4);

  // Phase 4: 0.34 - 0.50 (Image 2)
  const showImg2 = progress >= 0.34;
  const p4 = Math.min(1, Math.max(0, (progress - 0.34) / 0.16));
  const img2Scale = 0.2 + p4 * 0.8;
  const img2Opacity = Math.min(1, p4 * 4);

  // Phase 5: 0.50 - 0.64 (Image 3)
  const showImg3 = progress >= 0.50;
  const p5 = Math.min(1, Math.max(0, (progress - 0.50) / 0.14));
  const img3Scale = 0.2 + p5 * 0.8;
  const img3Opacity = Math.min(1, p5 * 4);

  // Phase 6: 0.64 - 0.78 (Dark Text screen)
  const showDarkText = progress >= 0.64;
  const p6 = Math.min(1, Math.max(0, (progress - 0.64) / 0.14));
  const darkTextScale = 0.2 + p6 * 0.8;
  const darkTextBaseOpacity = Math.min(1, p6 * 4);

  // Phase 7: 0.78 - 0.90 (Background morphs #030712 to white)
  const isMorphing = progress >= 0.78;
  const p7 = Math.min(1, Math.max(0, (progress - 0.78) / 0.12));
  const r = Math.round(3 + p7 * (255 - 3));
  const g = Math.round(7 + p7 * (255 - 7));
  const b = Math.round(18 + p7 * (255 - 18));
  const darkTextFadeOut = 1 - p7;
  const darkTextOpacity = darkTextBaseOpacity * darkTextFadeOut;

  // Phase 8: 0.90 - 1.00 (White screen with on-chain proofs)
  const isWhitePhase = progress >= 0.90;
  const p8 = Math.min(1, Math.max(0, (progress - 0.90) / 0.10));

  // Determine current stage background color
  const bgColor = isMorphing
    ? isWhitePhase
      ? '#ffffff'
      : `rgb(${r}, ${g}, ${b})`
    : progress >= 0.06
    ? '#030712'
    : 'transparent';

  return (
    <div
      className="fixed inset-0 z-10 pointer-events-none flex items-center justify-center overflow-hidden"
      style={{ backgroundColor: bgColor }}
    >
      {/* 0.00 - 0.06 Center Vertical Line Growing to 50px */}
      {isPhase1 && (
        <div
          className="absolute inset-y-0 bg-[#030712]"
          style={{
            width: `${line1Width}px`,
            left: '50%',
            transform: 'translateX(-50%)',
          }}
        />
      )}

      {/* 0.06 - 0.16 Center Line Expands to full screen width */}
      {progress >= 0.06 && progress < 0.16 && (
        <div
          className="absolute inset-y-0 bg-[#030712]"
          style={{
            width: `${50 + p2Eased * (window.innerWidth - 50)}px`,
            left: '50%',
            transform: 'translateX(-50%)',
          }}
        />
      )}

      {/* Image 1: Commercial Office */}
      {showImg1 && progress < 0.50 && (
        <div
          className="absolute inset-0 flex items-center justify-center p-4 sm:p-0"
          style={{
            transform: `scale(${img1Scale})`,
            opacity: img1Opacity,
            willChange: 'transform, opacity',
          }}
        >
          <div className="relative w-[85vw] h-auto max-w-[900px] aspect-[16/10] sm:w-full sm:h-full sm:max-w-none sm:aspect-auto rounded-none border border-white/10 sm:border-0 overflow-hidden bg-ink">
            <img
              src={IMAGES[0].url}
              alt={IMAGES[0].alt}
              className="w-full h-full object-cover rounded-none"
            />
            <div className="absolute inset-0 bg-gradient-to-t from-ink/80 via-transparent to-ink/20" />
            <div className="absolute bottom-6 left-6 font-geist text-[11px] tracking-[0.25em] uppercase text-white/80">
              {IMAGES[0].label}
            </div>
          </div>
        </div>
      )}

      {/* Image 2: Cyber Park */}
      {showImg2 && progress < 0.64 && (
        <div
          className="absolute inset-0 flex items-center justify-center p-4 sm:p-0"
          style={{
            transform: `scale(${img2Scale})`,
            opacity: img2Opacity,
            willChange: 'transform, opacity',
          }}
        >
          <div className="relative w-[85vw] h-auto max-w-[900px] aspect-[16/10] sm:w-full sm:h-full sm:max-w-none sm:aspect-auto rounded-none border border-white/10 sm:border-0 overflow-hidden bg-ink">
            <img
              src={IMAGES[1].url}
              alt={IMAGES[1].alt}
              className="w-full h-full object-cover rounded-none"
            />
            <div className="absolute inset-0 bg-gradient-to-t from-ink/80 via-transparent to-ink/20" />
            <div className="absolute bottom-6 left-6 font-geist text-[11px] tracking-[0.25em] uppercase text-white/80">
              {IMAGES[1].label}
            </div>
          </div>
        </div>
      )}

      {/* Image 3: Logistics Hub */}
      {showImg3 && progress < 0.78 && (
        <div
          className="absolute inset-0 flex items-center justify-center p-4 sm:p-0"
          style={{
            transform: `scale(${img3Scale})`,
            opacity: img3Opacity,
            willChange: 'transform, opacity',
          }}
        >
          <div className="relative w-[85vw] h-auto max-w-[900px] aspect-[16/10] sm:w-full sm:h-full sm:max-w-none sm:aspect-auto rounded-none border border-white/10 sm:border-0 overflow-hidden bg-ink">
            <img
              src={IMAGES[2].url}
              alt={IMAGES[2].alt}
              className="w-full h-full object-cover rounded-none"
            />
            <div className="absolute inset-0 bg-gradient-to-t from-ink/80 via-transparent to-ink/20" />
            <div className="absolute bottom-6 left-6 font-geist text-[11px] tracking-[0.25em] uppercase text-white/80">
              {IMAGES[2].label}
            </div>
          </div>
        </div>
      )}

      {/* 0.64 - 0.78: Dark Text Screen */}
      {showDarkText && darkTextOpacity > 0 && (
        <div
          className="absolute inset-0 flex items-center justify-center px-6"
          style={{
            transform: `scale(${darkTextScale})`,
            opacity: darkTextOpacity,
            willChange: 'transform, opacity',
          }}
        >
          <div className="max-w-4xl text-center">
            <h2 className="font-haboro text-3xl sm:text-5xl md:text-6xl text-white uppercase tracking-tight leading-[1.15]">
              Institutional property, settled through{' '}
              <span className="text-amber-500 font-bold">verified bank liens.</span>
            </h2>
            <p className="mt-6 font-geist text-sm sm:text-base text-white/60 tracking-[0.1em] uppercase">
              Zero-Custody TradFi Escrow · Axis Trustee Statutory Governance
            </p>
          </div>
        </div>
      )}

      {/* 0.90 - 1.00: White Screen */}
      {isWhitePhase && (
        <div
          className="absolute inset-0 flex flex-col items-center justify-center px-6 bg-white"
          style={{
            opacity: p8,
            willChange: 'opacity',
          }}
        >
          <div className="max-w-4xl text-center">
            <h2 className="font-haboro text-3xl sm:text-5xl md:text-6xl text-[#030712] uppercase tracking-tight leading-[1.15]">
              Every unit, provable{' '}
              <span className="text-amber-500 font-bold">on-chain.</span>
            </h2>
            <div className="font-geist text-[11px] tracking-[0.3em] uppercase text-neutral-500 mt-6 border-t border-neutral-200 pt-6">
              SHA-256 MERKLE ROOTS · ETHEREUM SEPOLIA
            </div>
          </div>
        </div>
      )}
    </div>
  );
};
export default ScrollGallery;
