---
name: comebye-pages
description: Make a page the user reads in Comebye's Pages screen (comebye publish). Use for any plan, review, report, comparison, UI mockup, diagram, chart or small tool you hand to the user, and read it before writing the page's first line.
---

# Comebye pages

A page is how you hand the user something to look at: a plan with mockups,
a review, a chart, a diagram, a small tool. `comebye report`, `todo add` and
chat are for short text. Use a page for anything with structure, pictures or
layout.

The user reads pages in the Comebye app, in the theme they picked (Comebye
Light or Dark, Gruvbox, Nord and so on), on a desktop or a phone. Every page
is HTML built on the Comebye page template, so it looks like the app.

## When to make one

- **Make a page for:**
  - a design or plan the user must approve;
  - anything with a UI mockup, a diagram or a chart;
  - a table wider than three columns;
  - a review with many findings;
  - a long investigation.
- **Don't make one for** a short answer, a status line or a one-paragraph
  report.
- **One subject per page.** Update it in place (see "Publish and update")
  instead of publishing a second page about the same thing.

## Start from a starter

```sh
comebye pages new --type doc --title "Garage failover drill" > page.html
```

| Type | For |
|---|---|
| `doc` | a plan, review, report or investigation (the default) |
| `mockup` | proposed UI: a real HTML/CSS screen, before and after, phone and desktop |
| `deck` | a short walk-through, one slide per screen |
| `dashboard` | numbers: tiles, a chart, a status table |

Each starter is the Comebye page template plus a filled-in example of its
type. Replace the example content, and keep the template's two pieces:

1. **The token blocks.** These are the `:root` block and the
   `@media (prefers-color-scheme: dark)` block at the top of the `<style>`.
   They hold the app's own colours from `web/themes.js`.
2. **The theme listener.** This is the `<script>` at the end of the page. In
   the app, the Pages viewer posts
   `{type: "comebye-theme", scheme: "light"|"dark", tokens: {"--bg": "#1d2021", ...}}`
   into the page when it loads and whenever the user changes theme. The
   listener applies only the known token names with plain colour values, and
   sets `color-scheme`. Keep it as it is: don't edit it, and don't add a
   second listener.

## The page contract

1. **A full document** with `<meta charset="utf-8">`, the viewport meta, and a
   `<title>` that names the page in two to five words. The title is a name,
   not a summary ("Garage failover drill", not "Report: what happened when
   we tested failover").
2. **Colours come only from the tokens.** Use `var(--bg)`, `--panel`,
   `--panel2`, `--border`, `--text`, `--muted`, the accents (`--blue`,
   `--green`, `--orange`, `--red`, `--purple`, `--yellow`, `--teal`),
   `--on-accent`, `--code-bg` and `--code-text`, the chips (`--chip-blue`,
   `--chip-green`, `--chip-red`, `--chip-yellow`, `--chip-purple`), `--th-bg`,
   `--mark-bg`, and `--sh-md` / `--sh-lg` for shadows.
   - Never write a literal colour in a rule, an SVG `fill` or a script.
   - Use the accents by meaning: green is ok, red is an error or blocked,
     orange needs the user, yellow is a warning or draft, blue is the primary
     accent and info, purple is secondary.
3. **Follow the theme everywhere.**
   - Inline SVG takes `fill="var(--panel)"` and `stroke="var(--border)"`,
     which follow a theme change by themselves. Prefer SVG to canvas.
   - A canvas or a chart library reads colours with
     `getComputedStyle(document.documentElement).getPropertyValue("--blue")`
     and must redraw on a theme change. Watch it with
     `new MutationObserver(redraw).observe(document.documentElement, {attributes: true, attributeFilter: ["style"]})`
     and with `matchMedia("(prefers-color-scheme: dark)")`'s `change` event.
   - To know whether the page is dark, use
     `getComputedStyle(document.documentElement).colorScheme`.
4. **Phone first.** The page must work at 390 px wide.
   - The template's `.wrap` gives a 16 px gutter.
   - The body must never scroll sideways.
   - Grids drop to one column on a phone. Give flex and grid children that
     hold text `min-width: 0`.
   - Wide tables go in `<div class="tablewrap">`, and wide diagrams and code
     go in their own `overflow-x: auto` box.
5. **Self-contained.** The page runs in a sandbox with no network:
   - **Scripts:** inline, or from `https://cdnjs.cloudflare.com/ajax/libs/<lib>/<exact version>/<file>`
     only, pinned to an exact version (see the table below). jsDelivr, unpkg,
     esm.sh and ES module imports from a CDN do not load.
   - **Styles:** inline only. There are no external stylesheets and no Google
     Fonts. Use the template's `--sans` and `--mono` stacks.
   - **Images and fonts:** `data:` URIs or inline SVG.
   - **Blocked:** `fetch`, XHR, WebSocket, `localStorage`, `sessionStorage`,
     cookies, forms that post, `alert`, `confirm`, `prompt`, `print()` and
     downloads all fail or do nothing. Keep data inline, for example in
     `<script type="application/json" id="data">`.
   - **Links to other sites** don't open from inside the app's frame yet.
     Write the URL as visible text next to the link. `#anchor` links inside
     the page work.
6. **Size:** 5 MiB at most. Keep screenshots at WebP or JPEG quality 70 to
   80, 960 px wide or less, which is about 30 to 120 KB each.
7. **Accessible:**
   - headings in order;
   - `alt` text on images;
   - visible focus (`outline: 2px solid var(--focus)`);
   - text contrast of 4.5:1 or more, which the tokens give you if you pair
     them as the template does;
   - `prefers-reduced-motion` respected.

## Mockups (the most valued kind of page)

Pick the strongest kind that fits:

1. **Real HTML/CSS components.**
   - Build the proposed screen as real markup styled with the tokens, so it
     looks like the app in any theme.
   - Put it in a frame with a caption that says "Mockup", so nobody mistakes
     it for the shipped UI.
   - Make it interactive where that helps: tabs that switch, a panel that
     opens.
   - Use realistic example data (agent names, ids, sizes) and label it as an
     example.
2. **Before and after.** Show a real screenshot of today's screen next to the
   mockup of the change.
   - In the comebye repo: `tools/uitest/dev.sh start`, then
     `node tools/uitest/shot.mjs '#/pages' --width 1280 --dark --out x.png`,
     then `magick x.png -resize 960x -quality 72 x.webp`.
   - Inline the WebP as a `data:image/webp;base64,...` URI.
   - Never screenshot anything with secrets on it.
3. **An inline SVG wireframe** when only the layout matters: boxes, labels and
   arrows. Use a `viewBox` with room for the labels, `width="100%"`, and
   colours from tokens.
4. **Phone and desktop side by side** for any screen whose layout changes:
   a 390 px frame and a wide frame, each in its own `overflow-x: auto` box.

Number the changes on the mockup with small callouts, and explain each
number in a list below it.

## Diagrams

- **Architecture, data flow and trust boundaries:** hand-written inline SVG.
  - a box per component;
  - a dashed box per trust boundary or origin;
  - arrows with short labels;
  - a red cross on a path that is blocked.

  Draw the real mechanism: the actual routes, files and ports, not generic
  boxes.
- **Sequences and state machines:** Mermaid from cdnjs
  (`mermaid/10.9.1/mermaid.min.js`). Call `mermaid.initialize({startOnLoad: false, securityLevel: "strict", theme: dark ? "dark" : "default"})`,
  then `mermaid.run()` on `<pre class="mermaid">` blocks, and render again on
  a theme change. Keep the source text so the diagram still reads if the
  script fails.

## Charts

- Chart real numbers only, drawn to scale, with axes labelled with units.
- For small charts, compute inline SVG in a script. The `dashboard` starter
  has an example that follows the theme without redrawing.
- For larger or interactive charts, use Chart.js from cdnjs
  (`Chart.js/4.4.1/chart.umd.min.js`) with colours read from the tokens, and
  redraw on a theme change.

## Pinned libraries (cdnjs)

| Library | Path after `https://cdnjs.cloudflare.com/ajax/libs/` |
|---|---|
| Chart.js | `Chart.js/4.4.1/chart.umd.min.js` |
| D3 | `d3/7.8.5/d3.min.js` |
| Mermaid | `mermaid/10.9.1/mermaid.min.js` |
| highlight.js | `highlight.js/11.9.0/highlight.min.js` (inline a theme's CSS, written with tokens) |
| marked | `marked/12.0.2/marked.min.js` |

Fonts that a library loads from a CDN (KaTeX, icon fonts) will not load.

## Writing

- Lead with the answer: put the recommendation or the result in the first
  screen, then the evidence.
- Write plain words and short sentences, and use the user's names for
  things.
- End with the decisions the user must make, numbered, each with its
  options and your recommendation.

## Look before you publish

If you can view images, look at the page at 390 px and at 1280 px, in light
and in dark, before you publish. The headless Chrome command:

```sh
google-chrome --headless --hide-scrollbars --window-size=390,1400 --screenshot=/tmp/p390.png "file://$PWD/page.html"
```

Then open the PNG. Add `--force-dark-mode` for dark. A `file://` page does
not get the hub's sandbox, so also keep to the rules in "Self-contained".
Fix what you see once, then publish.

## Publish and update

Give each page a stable key that starts with your agent name, so it can't
collide with another agent's page. Use the same key for every revision:

```sh
comebye publish page.html --key <your-name>-<subject>                  # first time and every update: same link
comebye publish page.html --key <your-name>-<subject> --if-version 3   # refuse if it changed since v3
```

`comebye whoami` shows your name, and `--key` takes letters, digits, `.`,
`_` and `-`. Every revision lands on the same link and keeps its version
history. Put the printed link in your report.
