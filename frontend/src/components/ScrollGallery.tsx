import type { CSSProperties, FC, ReactNode } from 'react';

interface ScrollGalleryProps {
  /** Scroll progress over the 900vh stage, clamped to 0–1. */
  progress: number;
  /** False once the content block has fully covered the viewport. */
  visible?: boolean;
}

interface GallerySlide {
  url: string;
  alt: string;
  /** Asset tag shown as a small caption in the corner of the photograph. */
  label: string;
  /** Step eyebrow above the headline. */
  step: string;
  /** Headline; wrap the accent words in <Accent>. */
  title: ReactNode;
  body: string;
}

const Accent: FC<{ children: ReactNode }> = ({ children }) => (
  <em className="italic font-normal text-amber-500">{children}</em>
);

// Photographs are bundled locally (public/images) so a third-party hotlink can never 404 mid-scroll.
const SLIDES: GallerySlide[] = [
  {
    url: '/images/gallery-01.jpg',
    alt: 'Commercial office tower, SPV 01',
    label: 'COMMERCIAL OFFICE · SPV 01',
    step: '01 · ASBA bank escrow',
    title: (
      <>
        Your capital never <Accent>leaves your bank.</Accent>
      </>
    ),
    body: 'Bids are blocked under a verified ASBA lien in the investor’s own account until allotment. AcreSync never custodies a rupee.',
  },
  {
    url: '/images/gallery-02.webp',
    alt: 'Cyber park, SPV 02',
    label: 'CYBER PARK · SPV 02',
    step: '02 · Statutory governance',
    title: (
      <>
        Structured under <Accent>Axis Trustee.</Accent>
      </>
    ),
    body: 'Every SPV scheme is sponsored, valued and offered under SEBI SM-REIT rules, with a statutory trustee over every rupee and every unit.',
  },
  {
    url: '/images/gallery-03.jpg',
    alt: 'Logistics hub, SPV 03',
    label: 'LOGISTICS HUB · SPV 03',
    step: '03 · On-chain audit',
    title: (
      <>
        Audited on-chain, <Accent>by anyone.</Accent>
      </>
    ),
    body: 'Allotments are committed as SHA-256 Merkle roots on Ethereum Sepolia, so any unitholder, auditor or regulator can recompute their proof.',
  },
];

/** #030712 */
const INK = { r: 3, g: 7, b: 18 };

const clamp01 = (v: number) => Math.min(1, Math.max(0, v));

/** Local 0–1 progress of a phase that runs from `start` to `end` of the stage. */
const phase = (progress: number, start: number, end: number) =>
  clamp01((progress - start) / (end - start));

const easeInOutCubic = (t: number) =>
  t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2;

const easeOutCubic = (t: number) => 1 - Math.pow(1 - t, 3);

/**
 * Shared "scale from centre" animation. The scale is eased so each layer settles into place
 * rather than stopping dead, the opacity fades over the first third of the phase, `settle` is a
 * slow inner zoom (1.12 → 1) on the photograph itself for depth, and `reveal` brings the slide's
 * caption block in once the photograph has (nearly) settled.
 */
const scaleIn = (p: number) => {
  const e = easeOutCubic(p);
  return {
    scale: 0.2 + e * 0.8,
    opacity: Math.min(1, p * 3),
    settle: 1.12 - e * 0.12,
    reveal: clamp01((p - 0.5) / 0.35),
  };
};

interface GalleryImageProps {
  slide: GallerySlide;
  /** Whether this slide's phase is running. Inactive slides stay mounted (hidden) so they are decoded before they are needed. */
  active: boolean;
  scale: number;
  opacity: number;
  /** Inner zoom applied to the photograph (see scaleIn). */
  settle: number;
  /** 0–1 entrance of the caption block (see scaleIn). */
  reveal: number;
  /** Opacity of the caption block: the entrance above, minus the fade-out once the next slide starts. */
  textOpacity: number;
}

const GalleryImage: FC<GalleryImageProps> = ({ slide, active, scale, opacity, settle, reveal, textOpacity }) => (
  <div
    className="absolute inset-0 flex flex-col items-center justify-center"
    style={{
      visibility: active ? 'visible' : 'hidden',
      transform: `scale(${scale})`,
      opacity,
      willChange: 'transform, opacity',
    }}
  >
    <div className="relative w-[85vw] h-auto max-w-[900px] aspect-[16/10] sm:w-full sm:h-full sm:max-w-none sm:aspect-auto rounded-md border border-white/10 sm:rounded-none sm:border-0 overflow-hidden bg-ink">
      <img
        src={slide.url}
        alt={slide.alt}
        decoding="async"
        className="w-full h-full object-cover"
        style={{ transform: `scale(${settle})`, willChange: 'transform' }}
      />
      <div className="absolute inset-0 bg-gradient-to-t from-ink/90 via-ink/30 to-ink/10" />
      <div className="absolute bottom-6 left-6 sm:left-12 md:left-16 font-ui text-[11px] tracking-[0.25em] uppercase text-white/60">
        {slide.label}
      </div>
    </div>

    {/* The product story for this slide: below the card on phones, over the photograph from sm up */}
    <div
      className="mt-6 w-[85vw] max-w-[900px] sm:mt-0 sm:absolute sm:left-12 md:left-16 sm:bottom-24 md:bottom-28 sm:w-auto sm:max-w-xl"
      style={{ opacity: textOpacity, transform: `translateY(${(1 - reveal) * 24}px)` }}
    >
      <div className="font-ui font-medium text-[11px] tracking-[0.3em] uppercase text-amber-500 mb-3">{slide.step}</div>
      <h3 className="font-display font-medium text-3xl sm:text-5xl md:text-6xl text-white leading-[1.05] tracking-tight">
        {slide.title}
      </h3>
      <p className="mt-4 font-ui font-light text-sm sm:text-base text-white/70 max-w-md">{slide.body}</p>
    </div>
  </div>
);

export const ScrollGallery: FC<ScrollGalleryProps> = ({ progress, visible = true }) => {
  // 0.00 – 0.06: a hairline grows from 0 to 50px at the centre of the viewport.
  const p1 = phase(progress, 0, 0.06);
  // 0.06 – 0.16: the line expands to the full viewport width (cubic ease-in-out).
  const p2 = easeInOutCubic(phase(progress, 0.06, 0.16));
  const viewportWidth = typeof window === 'undefined' ? 0 : window.innerWidth;
  const lineWidth = progress < 0.06 ? p1 * 50 : 50 + p2 * (viewportWidth - 50);

  // 0.16 – 0.34 / 0.34 – 0.50 / 0.50 – 0.64: the three slides scale in, each stacked on the last.
  const slide1 = scaleIn(phase(progress, 0.16, 0.34));
  const slide2 = scaleIn(phase(progress, 0.34, 0.5));
  const slide3 = scaleIn(phase(progress, 0.5, 0.64));

  // 0.64 – 0.78: a navy-black text screen scales in over the last slide.
  const dark = scaleIn(phase(progress, 0.64, 0.78));

  // A slide's caption fades out as soon as the next layer starts to scale in, so stacked captions
  // never overlap on phones, where the next card sits above the previous caption rather than over it.
  const exitBy = (nextStart: number, nextEnd: number) => 1 - clamp01(phase(progress, nextStart, nextEnd) / 0.2);

  // 0.78 – 0.90: the background morphs #030712 → white while the dark screen fades out.
  const p7 = phase(progress, 0.78, 0.9);
  const darkOpacity = dark.opacity * (1 - p7);

  // 0.90 – 1.00: the white screen fades in.
  const p8 = phase(progress, 0.9, 1);

  // The overlay stays transparent until the line has filled the viewport, otherwise the
  // line animation would play navy-on-navy and be invisible.
  let backgroundColor: string;
  if (progress < 0.16) {
    backgroundColor = 'transparent';
  } else if (progress < 0.78) {
    backgroundColor = '#030712';
  } else if (progress < 0.9) {
    const r = Math.round(INK.r + p7 * (255 - INK.r));
    const g = Math.round(INK.g + p7 * (255 - INK.g));
    const b = Math.round(INK.b + p7 * (255 - INK.b));
    backgroundColor = `rgb(${r}, ${g}, ${b})`;
  } else {
    backgroundColor = '#ffffff';
  }

  const containerStyle: CSSProperties = {
    backgroundColor,
    visibility: visible ? 'visible' : 'hidden',
  };

  return (
    <div
      className="fixed inset-0 z-10 pointer-events-none flex items-center justify-center overflow-hidden"
      style={containerStyle}
    >
      {/* 0.00 – 0.16: centre line grows to 50px, then expands to the full viewport */}
      {progress > 0 && progress < 0.16 && (
        <div
          className="absolute inset-y-0 left-1/2 -translate-x-1/2 bg-ink"
          style={{ width: `${lineWidth}px` }}
        />
      )}

      {/* Slides stay mounted so their photographs are decoded before their phase starts */}
      <GalleryImage
        slide={SLIDES[0]}
        active={progress >= 0.16 && progress < 0.5}
        {...slide1}
        textOpacity={slide1.reveal * exitBy(0.34, 0.5)}
      />
      <GalleryImage
        slide={SLIDES[1]}
        active={progress >= 0.34 && progress < 0.64}
        {...slide2}
        textOpacity={slide2.reveal * exitBy(0.5, 0.64)}
      />
      <GalleryImage
        slide={SLIDES[2]}
        active={progress >= 0.5 && progress < 0.78}
        {...slide3}
        textOpacity={slide3.reveal * exitBy(0.64, 0.78)}
      />

      {/* 0.64 – 0.90: navy-black text screen scales in, then fades out during the morph */}
      {progress >= 0.64 && darkOpacity > 0 && (
        <div
          className="absolute inset-0 flex items-center justify-center px-6 bg-ink"
          style={{
            transform: `scale(${dark.scale})`,
            opacity: darkOpacity,
            willChange: 'transform, opacity',
          }}
        >
          <div className="max-w-4xl text-center">
            <h2 className="font-display font-medium text-4xl sm:text-6xl md:text-7xl text-white tracking-tight leading-[1.05]">
              Institutional property, settled through <Accent>verified bank liens.</Accent>
            </h2>
            <p className="mt-6 font-ui text-sm sm:text-base text-white/60 tracking-[0.1em] uppercase">
              Zero-Custody TradFi Escrow · Axis Trustee Statutory Governance
            </p>
          </div>
        </div>
      )}

      {/* 0.90 – 1.00: white screen */}
      {progress >= 0.9 && (
        <div
          className="absolute inset-0 flex flex-col items-center justify-center px-6 bg-white"
          style={{ opacity: p8, willChange: 'opacity' }}
        >
          <div className="max-w-4xl text-center">
            <h2 className="font-display font-medium text-4xl sm:text-6xl md:text-7xl text-[#030712] tracking-tight leading-[1.05]">
              Every unit, provable{' '}
              <Accent>
                <span className="whitespace-nowrap">on-chain.</span>
              </Accent>
            </h2>
            <div className="font-ui text-[11px] tracking-[0.3em] uppercase text-neutral-500 mt-6 border-t border-neutral-200 pt-6">
              SHA-256 MERKLE ROOTS · ETHEREUM SEPOLIA
            </div>
          </div>
        </div>
      )}
    </div>
  );
};

export default ScrollGallery;
