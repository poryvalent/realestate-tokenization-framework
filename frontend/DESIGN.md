# Design

<!-- impeccable:design-schema 1 -->

## World

Dark cinematic attestation console: a near-black `#020319` void with a deep-indigo
glow, glass-pill navigation and liquid-glass data cards. Colour does jobs — action
(`#BEC7FF` mist), evidence (mono hashes, Etherscan links), money (tabular `en-IN`
rupees), danger (rose) — never decoration. Display voice is Inter Tight
(400–700, offline fallback to system-ui); data speaks in tabular numerals;
IBM Plex Mono is reserved for hashes, addresses and code. Giant gradient titles
(`#A8B4FF → #FFFFFF`) sit behind a grid-and-glow parallax field with film grain.

## Layout

Centered column (`max-w-6xl`) floating over a fixed ambient field (grid + indigo
glow + grain). Glass pill topbar (fixed, rounded-full, blur 18px) with mobile
fullscreen overlay menu. Sections stack as liquid-glass cards with 16–24px radii;
only single-column forms (bid, sign-in fields) use `.narrow` (680px). Footer is a
thin ruled band with the Sepolia contract link and the risk disclaimer.

## Type

Hero display `clamp(2.5rem, 6vw, 4rem)` Inter Tight semibold, tight; section heads
semibold with eyebrow kickers in tracked uppercase; body capped at 68–72ch in
`white/60`. Figures are oversized tabular numerals. Mono (`IBM Plex Mono`) only
for digests, addresses, Tx hashes and code.

## Motion

One authored moment per region, all Framer Motion: blur-fade rise entrances with
stagger, count-up figures, infinite marquee ticker, AnimatePresence route fades,
glass-pill mobile menu with staggered link entrances, accordion height animation,
scroll progress rule, brand preloader. Everything self-disables under
`prefers-reduced-motion`. Motion tokens: duration 0.3–0.6s, easing
`[0.25, 0.1, 0.25, 1]`.
