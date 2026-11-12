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
