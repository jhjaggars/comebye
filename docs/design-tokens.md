# Design tokens

Colour tokens live in `web/themes.js` (15 themes, checked by `tools/uitest/contrast.mjs`).
The non-colour tokens live in `:root` at the top of the `<style>` block in `web/index.html`,
next to `--safe-bottom`. There is no build step: they are plain CSS custom properties.

## The scales

| Scale | Tokens | Values (px) | Use for |
|---|---|---|---|
| Space | `--sp-N` | 2 4 6 8 10 12 14 16 20 24 32 | `padding`, `margin`, `gap` |
| Radius | `--r-N`, `--r-pill` | 2 4 6 8 10 12 14 16, pill (999) | `border-radius` |
| Type | `--fs-N` | 11 12 13 14 15 16 18 20 22 24 | `font-size` (and the size in `font:`) |
| Hit size | `--hit-N` | 32 36 40 44 48 | `min-height` of anything you tap or click |
| Focus ring | `--ring`, `--ring-off` | 2px solid in the theme's `--focus`, 2px offset | one global `:focus-visible` rule; a component only overrides `outline-offset` (inset `-1px`/`-2px` where the ring would be clipped) |
| Elevation | `--elev-1`, `--elev-2` | card shadow, popover shadow | `box-shadow` |

The names are the pixel value, so a token reads the same as the number it replaces:
`padding: var(--sp-8) var(--sp-12)`, `border-radius: var(--r-10)`, `font-size: var(--fs-13)`.
`0` and `1px` (hairline borders, 1px insets) stay literal, and so do negative margins, which
cannot be a `var()`. `50%` stays for circles.

Hit sizes are defined but not adopted yet: component by component, `min-height: 44px` becomes
`var(--hit-44)`. Phone controls are 44 (the iOS minimum), dense desktop controls 32-36.
Not tokenised yet: `z-index` (13 distinct values), breakpoints (9), transitions.

## Rules

1. In `padding`, `margin`, `gap`, `border-radius` and `font-size`, write the token, not the number.
2. Need a value that is not on a scale? Pick the nearest step. Add a step only with a reason in the PR;
   the scale is meant to stay small.
3. `tools/uitest/tokens.mjs` enforces this as a ratchet (below). Colours still come from `themes.js` only.

## The ratchet

```sh
node tools/uitest/tokens.mjs            # check (CI runs it): exit 1 if a count went up
node tools/uitest/tokens.mjs --verbose  # list every offender, with its selector
node tools/uitest/tokens.mjs --update   # lower tools/uitest/tokens-baseline.json after you fixed some
```

Per scale it counts `literal` (a positive px written out where a token exists) and `offscale`
(a px on no scale). `tokens-baseline.json` holds today's counts; the check fails if either goes
up, and also when it goes down, to make you commit the lower baseline. `--update` refuses to raise
a count. The scales it checks against are read from `:root`, so adding a step there is one edit.

## The snap (what moved)

The first pass replaced literals with tokens. Values already on a scale are an exact swap (no pixel
changes). Values within 1px of a step moved to it (ties round up): spacing 3/5/7/9/11/13 to
4/6/8/10/12/14, radius 3/5/7/9/11 to 4/6/8/10/12 and 99px to the pill, type 10.5/11.5/12.5/13.5/14.5
to 11/12/13/14/15 and 10/17/19/21 to 11/18/20/22. Anything 2px or more from a step (for example
`padding: 18px`, `font-size: 9px`, `border-radius: 22px`) was left alone and counts as `offscale`.

### Type snap (slice 3b)

Every `font-size` and `font:` size in the stylesheet now uses `--fs-N`. Same rule as above: 10.5/11.5/12.5/13.5/14.5
went up to 11/12/13/14/15, 10/17/19/21 to 11/18/20/22. Only 9px and 26px stay literal (offscale 2). The font family
was not touched.

### Buttons and the focus ring (slice 4)

`:focus-visible` has one rule at the top of the stylesheet (`outline: var(--ring)`). Component rules no longer repeat
the colour or width; the 40 that need a different offset set `outline-offset` alone, the 12 that matched the default are gone,
and the `--blue` rings now use the theme's `--focus`. Every `min-height` of 32/36/40/44/48px is `var(--hit-N)` (ratchet: `hit.literal`).
`.btn` has one tone set (`.primary`, `.warn`, `.danger`) and a dense size (`.btn.sm`, 32px). Per-surface button rules (`.td-acts button`,
`.bd-seg button`, ...) are not folded into `.btn` yet: they are different controls (segments, menu items) and belong to slices 5 and 6.

### Chips, cards, empty states (slice 5)

*Chip:* nine chip classes (`.wt-chip .host-chip .lg-chip .role-chip .us-chip .bd-tag .td-chip .rel-chip`, plus the new `.chip`) share one
shape (6px radius, `1px 6px`, 11px, 600, no wrap; `.us-chip` and `.rel-chip` are 12px) and one tone table (blue default, `purple yellow green orange red teal muted`).
A chip class only adds layout; a new one is `.chip.green`. Not folded in: the filter pills (`.hf-chip`, `.rc-chip`, buttons), the
count badge `.bd-chip`, and the outlined `.rc-tag`/`.rv-chip` labels, which are different controls.
*Card:* one surface (`.card`, 1px hairline, 12px radius; `.attn` `.bad` `.good` set the edge). `.bcard .pz-card .rl-card .us-card` and the four
whole-card links (`.rc-card .pgs-card .pgs-hit .pr-card`) share it, and the link cards share one hover/ring/`.current` rule. `.bcard` went from 14px to 12px.
*Empty state:* `.pal-empty .pgs-empty .pr-empty .us-empty` join `.empty` (centred, muted, 28px/20px padding); `.ap-empty` (an inline italic note) is left alone.

### Segmented controls, menus and sheets, notices, stacking (slice 6)

*Segmented control:* `.seg` (and `.bd-seg .bc-seg .pgs-seg .us-seg`) is one hairline box with a 10px radius; the pressed segment (`aria-pressed`) is the accent.
A control adds only its own sizing (`.us-seg` is the dense 36px one). The underline tabs (`.pr-tabs`, `.sp-tabs`) are a different control and stay.
*Menu / sheet:* `.pop` was already the one popover-and-bottom-sheet primitive (grab handle, safe-area padding, backdrop); its radius is now `--r-16`
(was 18px) and its layers are tokens.
*Stacking:* `--z-raised 2, pop 5, bar 10, pill 15, view 30, backdrop 34, menu 35, popup 36, sheet 37, screen 40, scrim 70, toast 100, toast-over 101`.
A two-digit literal `z-index` is a ratchet failure (`z.literal`).
*Notice:* `#toast`, `.rtr-banner` and the briefing card (`.br-card`) share one 12px surface; `.notice.info|warn|err` sets the edge colour (and the warn tint).

### Shared with Pages (slice 7)

`internal/pageskit/template.html` (the Pages template and the four starters) now carries the same non-colour scales in a second `:root` block,
the one `:focus-visible` rule, and the app's `.chip` (radius 6, 12px size), `.card`, `.btn`, `.seg` and `.callout`. `TestTemplateSharesTheAppScales`
fails if the template's `--sp-/--r-/--fs-/--hit-/--elev-/--ring-off` values drift from `web/index.html`, or if the template or a starter writes a
radius or font size that is on, or within 1px of, a scale step as a literal. The component reference is a Page (title "Design language components").
Not shared (they need the app's scripts): the bottom sheet, toast, banner and rows.
