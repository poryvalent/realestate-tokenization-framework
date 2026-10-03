# Design

<!-- impeccable:design-schema 1 -->

## World

Exhibition registry: a full-bleed institutional ledger built for a live business-event showcase.
Paper ground, ink text, one deep-green action accent. Colour does jobs — action, evidence,
money, danger — never decoration. Display voice is a book serif (Georgia/Iowan stack,
offline-safe); data speaks in tabular numerals and small-caps labels; mono is reserved for
hashes, addresses and code.

## Layout

Fluid full-bleed page: `--gutter: clamp(20px, 5vw, 96px)` is the only margin, capped at
2000px. Sections own the viewport edge to edge — hero (86svh), ruled figure strip, ink
marquee ticker, sticky how-it-works, spotlight grid, full-bleed dark attestation band,
ledger scheme rows. Only single-column forms (bid, sign-in fields) use `.narrow` (680px).
Breakout sections (marquee, band) use negative gutter margins, never viewport hacks.

## Type

Hero display `clamp(3rem, 8.5vw, 6rem)` serif, tight; section heads serif with 3px rules;
body capped at 68–72ch. Figures are oversized tabular serif numerals with tracked
small-caps labels. Kickers/eyebrows are banned; headings carry their own weight.

## Motion

One authored moment per region: kinetic letter-stagger hero, scroll + pointer
parallax wash (transform-only, rAF), count-up figures, marquee ticker,
spotlight + 5° tilt cards, magnetic CTAs, difference-blend cursor (fine pointers only),
film grain, scroll progress rule, brand preloader, Lenis smooth scroll. Everything
self-disables under `prefers-reduced-motion` and coarse pointers. Tokens live in
`src/theme/motion.css`; snippets in `transitions.css`.
