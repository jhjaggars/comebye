/* Comebye themes: every theme is ONE entry in THEMES below.
 *
 * This file is loaded synchronously from <head> (before first paint, and as
 * an external script because the server's CSP is script-src 'self'): it
 * writes one CSS rule per theme, `[data-theme="<id>"] { ...custom properties }`, then
 * sets data-theme on <html> from the saved choice, so there is no flash of
 * the wrong theme. index.html holds no colours; it only uses var() of the tokens.
 *
 * To add a theme, add a `theme({...})` entry with a core palette (bg, panel,
 * panel2, border, text, muted and the seven accents). The other ~35 tokens
 * (chips, code, marks, shadows...) are derived. Any text colour that would
 * fall under WCAG AA (4.5:1) on the surfaces it sits on is nudged toward
 * the theme's text colour until it passes, and the nudge is recorded in
 * `adjusted` (node tools/uitest/contrast.mjs prints them). Add the entry
 * and run that script: it fails if any theme is under 4.5:1 (body, muted,
 * chip text, buttons) or 3:1 (borders and large UI), or lacks a token that
 * index.html uses.
 *
 * Choice (per browser, localStorage):
 *   comebye.theme        "system" | a theme id   ("light"/"dark" as before)
 *   comebye.theme.light  theme id used when the OS is light and choice is system
 *   comebye.theme.dark   theme id used when the OS is dark  and choice is system
 *
 * Palettes are the schemes' published colours (sources/licences at each
 * entry; all MIT unless noted). Mapping: scheme red -> --red (errors),
 * green -> --green (ok), yellow -> --yellow (warn/draft), blue -> --blue
 * (primary accent, info, "working"), orange -> --orange (needs you, blocked),
 * purple/magenta -> --purple (sub-agents), cyan/aqua -> --teal.
 */
(function (root) {
  "use strict";

  // ---------- colour maths ----------
  const hex2rgb = (h) => { h = h.replace("#", ""); if (h.length === 3) h = h.replace(/./g, "$&$&"); return [0, 2, 4].map((i) => parseInt(h.slice(i, i + 2), 16)); };
  const rgb2hex = (c) => "#" + c.map((x) => Math.max(0, Math.min(255, Math.round(x))).toString(16).padStart(2, "0")).join("");
  // mix(a, b, t): t of a, the rest b.
  const mix = (a, b, t) => { const x = hex2rgb(a), y = hex2rgb(b); return rgb2hex(x.map((v, i) => v * t + y[i] * (1 - t))); };
  const lum = (h) => { const [r, g, b] = hex2rgb(h).map((v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); }); return 0.2126 * r + 0.7152 * g + 0.0722 * b; };
  const contrast = (a, b) => { const x = lum(a), y = lum(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); };
  const rgba = (h, a) => `rgba(${hex2rgb(h).join(",")},${a})`;
  const best = (bg, cands) => cands.reduce((m, c) => (contrast(c, bg) > contrast(m, bg) ? c : m));

  // HSL lightness walk: keeps hue and saturation, so a failing orange becomes
  // a deeper orange, not a grey.
  const rgb2hsl = ([r, g, b]) => {
    r /= 255; g /= 255; b /= 255;
    const mx = Math.max(r, g, b), mn = Math.min(r, g, b), l = (mx + mn) / 2, d = mx - mn;
    if (!d) return [0, 0, l];
    const s = l > 0.5 ? d / (2 - mx - mn) : d / (mx + mn);
    const h = mx === r ? (g - b) / d + (g < b ? 6 : 0) : mx === g ? (b - r) / d + 2 : (r - g) / d + 4;
    return [h / 6, s, l];
  };
  const hsl2hex = ([h, s, l]) => {
    const f = (p, q, t) => { t = (t + 1) % 1; return t < 1 / 6 ? p + (q - p) * 6 * t : t < 1 / 2 ? q : t < 2 / 3 ? p + (q - p) * (2 / 3 - t) * 6 : p; };
    if (!s) return rgb2hex([l * 255, l * 255, l * 255]);
    const q = l < 0.5 ? l * (1 + s) : l + s - l * s, p = 2 * l - q;
    return rgb2hex([f(p, q, h + 1 / 3), f(p, q, h), f(p, q, h - 1 / 3)].map((x) => x * 255));
  };

  // theme() derives the full token set from a core palette.
  function theme(p) {
    const dark = p.mode === "dark";
    const adjusted = {};
    const { bg, panel, panel2, border } = p;
    const MIN = 4.5;
    // fit: walk colour c's lightness (away from the background: lighter on
    // dark themes, darker on light ones) until it has >= 4.5:1 on every
    // background bgsOf(c) returns (some are tints of c itself).
    const fit = (name, c, bgsOf) => {
      const hsl = rgb2hsl(hex2rgb(c));
      let cur = c;
      for (let i = 0; i <= 100; i++) {
        if (bgsOf(cur).every((b) => contrast(cur, b) >= MIN)) break;
        const l = hsl[2] + (dark ? 1 : -1) * 0.01 * i;
        cur = hsl2hex([hsl[0], hsl[1], Math.max(0, Math.min(1, l))]);
      }
      if (cur !== c) adjusted[name] = [c, cur];
      return cur;
    };
    // tint: a surface that is `r` of colour c over `base`, backed off until
    // the body text reads on it.
    const tint = (c, base, r, fg) => {
      let x = mix(c, base, r);
      while (r > 0.03 && contrast(fg, x) < MIN) { r -= 0.02; x = mix(c, base, r); }
      return contrast(fg, x) < MIN ? base : x;
    };
    const base = [bg, panel, panel2];
    const text = fit("text", p.text, () => base);
    const v = { bg, panel, panel2, border, text };
    const codeBg = p.code || (dark ? mix("#000000", bg, 0.5) : mix("#000000", bg, 0.04));
    v["code-bg"] = codeBg;
    v["code-text"] = text;
    v["rc-out"] = codeBg;
    v["th-bg"] = tint(border, panel2, 0.3, text);
    v["btn-bg"] = tint(border, panel2, 0.5, text);
    const surf = [...base, codeBg];
    v.muted = fit("muted", p.muted, () => surf);
    const ratio = dark ? 0.16 : 0.13;
        for (const k of ["blue", "orange", "green", "purple", "red", "yellow", "teal"]) {
      v[k] = fit(k, p[k], (c) => [...surf, mix(c, panel, ratio)]);
    }
    // Chip backgrounds (the foreground is the accent itself).
    const ch = (c) => mix(c, panel, ratio);
    v["chip-blue"] = ch(v.blue); v["chip-purple"] = ch(v.purple); v["chip-red"] = ch(v.red);
    v["chip-green"] = ch(v.green); v["chip-finished"] = ch(v.purple); v["chip-cand"] = ch(v.green);
    v["chip-rtr"] = ch(v.orange); v["chip-landing"] = ch(v.teal); v["chip-yellow"] = ch(v.yellow);
    // Text on a filled accent (send button, stop, badges).
    v["on-accent"] = best(v.blue, [bg, "#ffffff", "#000000"]);
    v["on-red"] = best(v.red, [bg, "#ffffff", "#000000"]);
    v["on-orange"] = best(v.orange, [bg, "#ffffff", "#000000"]);
    v.quote = fit("quote", mix(text, v.muted, 0.5), () => surf);
    v.snip = fit("snip", mix(text, v.muted, 0.7), () => surf);
    v["blocked-bg"] = tint(v.orange, panel, dark ? 0.12 : 0.1, text);
    v["blocked-name"] = fit("blocked-name", mix(v.orange, text, 0.45), () => [v["blocked-bg"]]);
    v.bubble = tint(v.blue, bg, dark ? 0.3 : 0.2, text);
    v["rc-user"] = tint(v.blue, bg, dark ? 0.22 : 0.16, text);
    v["mark-bg"] = tint(v.yellow, bg, dark ? 0.32 : 0.4, text);
    v["rcmark-bg"] = mix(v.yellow, bg, dark ? 0.4 : 0.5);
    v["rcmark-text"] = best(v["rcmark-bg"], [text, bg, "#000000", "#ffffff"]);
    v["secret-bg"] = rgba(v.orange, 0.12);
    v["sh-md"] = dark ? "rgba(0, 0, 0, 0.35)" : "rgba(30, 40, 70, 0.14)";
    v["sh-lg"] = dark ? "rgba(0, 0, 0, 0.5)" : "rgba(30, 40, 70, 0.22)";
    // Selection and the focus ring follow the primary accent.
    v.selection = tint(v.blue, bg, dark ? 0.35 : 0.25, text);
    v.focus = v.blue;
    return { id: p.id, name: p.name, mode: p.mode, src: p.src, vars: v, adjusted };
  }

  // The two original themes keep their exact hand-tuned values.
  const comebyeDark = {
    bg: "#101218", panel: "#181a20", panel2: "#1f222b", border: "#2a2e3a", text: "#e6e8ee", muted: "#8b91a0",
    blue: "#60a5fa", orange: "#fb923c", green: "#4ade80", purple: "#c084fc", red: "#f87171", yellow: "#e5c07b", teal: "#5eead4",
    "on-accent": "#0b0d11", "on-red": "#0b0d11", "on-orange": "#111",
    "code-bg": "#0b0d11", "code-text": "#cdd3e0", quote: "#b9bfcc", snip: "#c6cad5",
    "th-bg": "#262a35", "btn-bg": "#2a2e3a",
    "blocked-bg": "#231a12", "blocked-name": "#ffd2a8",
    bubble: "#2b4a7a", "rc-user": "#1e3a5f", "rc-out": "#0c0e13",
    "mark-bg": "#4a3f12", "rcmark-bg": "#5b4a12", "rcmark-text": "#ffe9a8",
    "chip-blue": "#14202f", "chip-purple": "#241a30", "chip-red": "#2f1414", "chip-green": "#12261c",
    "chip-finished": "#1d1730", "chip-cand": "#132a1c", "chip-rtr": "#2e1d10", "chip-landing": "#102a2c", "chip-yellow": "#2a2412",
    "secret-bg": "rgba(251,146,60,.12)", "sh-md": "rgba(0, 0, 0, 0.35)", "sh-lg": "rgba(0, 0, 0, 0.5)",
    selection: "#2b4a7a", focus: "#60a5fa",
  };
  const comebyeLight = {
    bg: "#f3f4f7", panel: "#ffffff", panel2: "#eceef3", border: "#d3d7e0", text: "#1a1d26", muted: "#596072",
    blue: "#1c58b8", orange: "#a84a06", green: "#136f33", purple: "#7e32c4", red: "#c42323", yellow: "#8a5a00", teal: "#0f766e",
    "on-accent": "#fff", "on-red": "#fff", "on-orange": "#fff",
    "code-bg": "#eef0f5", "code-text": "#272b36", quote: "#4a5163", snip: "#3b4152",
    "th-bg": "#e6e9f0", "btn-bg": "#e4e7ee",
    "blocked-bg": "#fff4e8", "blocked-name": "#7a3305",
    bubble: "#d6e6fb", "rc-user": "#dbe8fa", "rc-out": "#eef0f5",
    "mark-bg": "#fde68a", "rcmark-bg": "#fde68a", "rcmark-text": "#3d2c00",
    "chip-blue": "#e3edfb", "chip-purple": "#f0e6fb", "chip-red": "#fbe6e6", "chip-green": "#e0f3e6",
    "chip-finished": "#efe6fb", "chip-cand": "#e0f3e6", "chip-rtr": "#fcebdb", "chip-landing": "#dcf3f0", "chip-yellow": "#fbf0d0",
    "secret-bg": "rgba(168,74,6,.1)", "sh-md": "rgba(30, 40, 70, 0.14)", "sh-lg": "rgba(30, 40, 70, 0.22)",
    selection: "#d6e6fb", focus: "#1c58b8",
  };

  const THEMES = [
    { id: "dark", name: "Comebye Dark", mode: "dark", src: "Comebye", vars: comebyeDark, adjusted: {} },
    { id: "light", name: "Comebye Light", mode: "light", src: "Comebye", vars: comebyeLight, adjusted: {} },

    // Gruvbox by Pavel Pertsev (morhetz), MIT/X11, github.com/morhetz/gruvbox.
    // bg0_h/bg0/bg1/bg2, fg1, gray; bright accents on dark, faded on light.
    theme({ id: "gruvbox-dark", name: "Gruvbox Dark", mode: "dark", src: "morhetz/gruvbox (MIT)",
      bg: "#1d2021", panel: "#282828", panel2: "#3c3836", border: "#504945", text: "#ebdbb2", muted: "#a89984",
      blue: "#83a598", orange: "#fe8019", green: "#b8bb26", purple: "#d3869b", red: "#fb4934", yellow: "#fabd2f", teal: "#8ec07c" }),
    theme({ id: "gruvbox-light", name: "Gruvbox Light", mode: "light", src: "morhetz/gruvbox (MIT)",
      bg: "#fbf1c7", panel: "#f9f5d7", panel2: "#ebdbb2", border: "#d5c4a1", text: "#3c3836", muted: "#7c6f64",
      blue: "#076678", orange: "#af3a03", green: "#79740e", purple: "#8f3f71", red: "#9d0006", yellow: "#b57614", teal: "#427b58" }),

    // Dracula by Zeno Rocha, MIT, draculatheme.com / github.com/dracula/dracula-theme.
    // background, current line, selection, foreground, comment; pink/purple/cyan/green/orange/red/yellow.
    theme({ id: "dracula", name: "Dracula", mode: "dark", src: "dracula/dracula-theme (MIT)",
      bg: "#21222c", panel: "#282a36", panel2: "#343746", border: "#44475a", text: "#f8f8f2", muted: "#6272a4",
      blue: "#8be9fd", orange: "#ffb86c", green: "#50fa7b", purple: "#bd93f9", red: "#ff5555", yellow: "#f1fa8c", teal: "#ff79c6" }),

    // Tokyo Night by enkia, MIT, github.com/enkia/tokyo-night-vscode-theme
    // (values as in folke/tokyonight.nvim, MIT, which also defines Storm and Day).
    theme({ id: "tokyonight-night", name: "Tokyo Night", mode: "dark", src: "enkia/tokyo-night-vscode-theme (MIT)",
      bg: "#16161e", panel: "#1a1b26", panel2: "#292e42", border: "#3b4261", text: "#c0caf5", muted: "#737aa2",
      blue: "#7aa2f7", orange: "#ff9e64", green: "#9ece6a", purple: "#bb9af7", red: "#f7768e", yellow: "#e0af68", teal: "#7dcfff" }),
    theme({ id: "tokyonight-storm", name: "Tokyo Night Storm", mode: "dark", src: "enkia/tokyo-night-vscode-theme (MIT)",
      bg: "#1f2335", panel: "#24283b", panel2: "#2f3549", border: "#3b4261", text: "#c0caf5", muted: "#737aa2",
      blue: "#7aa2f7", orange: "#ff9e64", green: "#9ece6a", purple: "#bb9af7", red: "#f7768e", yellow: "#e0af68", teal: "#7dcfff" }),
    theme({ id: "tokyonight-day", name: "Tokyo Night Day", mode: "light", src: "folke/tokyonight.nvim day (MIT)",
      bg: "#e1e2e7", panel: "#e9e9ed", panel2: "#d0d5e3", border: "#a8aecb", text: "#3760bf", muted: "#6172b0",
      blue: "#2e7de9", orange: "#b15c00", green: "#587539", purple: "#9854f1", red: "#f52a65", yellow: "#8c6c3e", teal: "#118c74" }),

    // Catppuccin by the Catppuccin Org, MIT, github.com/catppuccin/catppuccin.
    // base/mantle/crust/surface0-2, text/subtext/overlay.
    theme({ id: "catppuccin-mocha", name: "Catppuccin Mocha", mode: "dark", src: "catppuccin/catppuccin (MIT)",
      bg: "#181825", panel: "#1e1e2e", panel2: "#313244", border: "#45475a", text: "#cdd6f4", muted: "#9399b2",
      blue: "#89b4fa", orange: "#fab387", green: "#a6e3a1", purple: "#cba6f7", red: "#f38ba8", yellow: "#f9e2af", teal: "#94e2d5" }),
    theme({ id: "catppuccin-latte", name: "Catppuccin Latte", mode: "light", src: "catppuccin/catppuccin (MIT)",
      bg: "#e6e9ef", panel: "#eff1f5", panel2: "#ccd0da", border: "#bcc0cc", text: "#4c4f69", muted: "#6c6f85",
      blue: "#1e66f5", orange: "#fe640b", green: "#40a02b", purple: "#8839ef", red: "#d20f39", yellow: "#df8e1d", teal: "#179299" }),

    // Nord by Arctic Ice Studio / Sven Greb, MIT, nordtheme.com.
    // nord0-3 Polar Night (bg is a darker #272c36, the common "Nord darker" step, so accents fit), nord4 Snow Storm, nord7-10 Frost, nord11-15 Aurora.
    theme({ id: "nord", name: "Nord", mode: "dark", src: "nordtheme/nord (MIT)",
      bg: "#272c36", panel: "#2e3440", panel2: "#3b4252", border: "#4c566a", text: "#eceff4", muted: "#a5b1c7",
      blue: "#88c0d0", orange: "#d08770", green: "#a3be8c", purple: "#b48ead", red: "#bf616a", yellow: "#ebcb8b", teal: "#8fbcbb" }),

    // Solarized by Ethan Schoonover, MIT, ethanschoonover.com/solarized.
    // base03..base3 and the eight accents; muted is base01/base1.
    theme({ id: "solarized-dark", name: "Solarized Dark", mode: "dark", src: "altercation/solarized (MIT)",
      bg: "#002b36", panel: "#073642", panel2: "#0d4452", border: "#586e75", text: "#93a1a1", muted: "#839496",
      blue: "#268bd2", orange: "#cb4b16", green: "#859900", purple: "#6c71c4", red: "#dc322f", yellow: "#b58900", teal: "#2aa198" }),
    theme({ id: "solarized-light", name: "Solarized Light", mode: "light", src: "altercation/solarized (MIT)",
      bg: "#eee8d5", panel: "#fdf6e3", panel2: "#e4ddc8", border: "#93a1a1", text: "#586e75", muted: "#657b83",
      blue: "#268bd2", orange: "#cb4b16", green: "#859900", purple: "#6c71c4", red: "#dc322f", yellow: "#b58900", teal: "#2aa198" }),

    // One Dark from Atom's One Dark syntax theme (Atom, MIT), github.com/atom/atom/tree/master/packages/one-dark-syntax.
    // mono-1 text, hue-1..6 accents; backgrounds from the One Dark UI theme.
    theme({ id: "one-dark", name: "One Dark", mode: "dark", src: "atom/one-dark-syntax (MIT)",
      bg: "#21252b", panel: "#282c34", panel2: "#2c313c", border: "#3e4451", text: "#abb2bf", muted: "#7f848e",
      blue: "#61afef", orange: "#d19a66", green: "#98c379", purple: "#c678dd", red: "#e06c75", yellow: "#e5c07b", teal: "#56b6c2" }),

    // Rosé Pine by Rosé Pine, MIT, rosepinetheme.com. Main variant: base,
    // surface, overlay, highlight med; text/subtle/muted. It has no blue or
    // green: foam -> blue, pine -> green (lightened to pass AA), iris -> purple.
    theme({ id: "rose-pine", name: "Rosé Pine", mode: "dark", src: "rose-pine/rose-pine-theme (MIT)",
      bg: "#191724", panel: "#1f1d2e", panel2: "#26233a", border: "#403d52", text: "#e0def4", muted: "#908caa",
      blue: "#9ccfd8", orange: "#f6c177", green: "#31748f", purple: "#c4a7e7", red: "#eb6f92", yellow: "#ebbcba", teal: "#9ccfd8" }),
  ];

  const byId = {};
  THEMES.forEach((t) => { byId[t.id] = t; });

  const API = { THEMES, byId, mix, contrast, KEY: "comebye.theme", KEY_LIGHT: "comebye.theme.light", KEY_DARK: "comebye.theme.dark" };
  root.ComebyeThemes = API;
  if (typeof document === "undefined") return; // node (tools/uitest/contrast.mjs)

  // ---------- CSS: one rule per theme ----------
  const css = THEMES.map((t) =>
    `[data-theme="${t.id}"]{color-scheme:${t.mode};` + Object.keys(t.vars).map((k) => `--${k}:${t.vars[k]}`).join(";") + "}").join("\n");
  const st = document.createElement("style");
  st.id = "theme-vars";
  st.textContent = css;
  document.head.appendChild(st);

  // ---------- choice + apply ----------
  const ls = {
    get(k) { try { return localStorage.getItem(k); } catch (e) { return null; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch (e) {} },
  };
  const mq = root.matchMedia ? root.matchMedia("(prefers-color-scheme: light)") : null;
  const osLight = () => !!(mq && mq.matches);

  API.choice = () => { const t = ls.get(API.KEY); return t === "system" || byId[t] ? t : "system"; };
  // sysPair: the theme ids used for each OS mode while the choice is "system".
  API.sysPair = () => {
    const l = ls.get(API.KEY_LIGHT), d = ls.get(API.KEY_DARK);
    return { light: byId[l] && byId[l].mode === "light" ? l : "light", dark: byId[d] && byId[d].mode === "dark" ? d : "dark" };
  };
  API.resolve = (choice) => {
    choice = choice || API.choice();
    if (choice !== "system") return choice;
    const p = API.sysPair();
    return osLight() ? p.light : p.dark;
  };
  API.apply = () => {
    const id = API.resolve();
    const el = document.documentElement;
    if (el.getAttribute("data-theme") !== id) el.setAttribute("data-theme", id);
    // One theme-color for the browser chrome (the two media-qualified metas
    // in index.html are the no-JS fallback).
    const metas = document.querySelectorAll('meta[name="theme-color"]');
    metas.forEach((m, i) => {
      if (i === 0) { m.setAttribute("content", byId[id].vars.bg); m.removeAttribute("media"); } else m.remove();
    });
    return id;
  };
  // preview paints a theme for a moment without saving it; apply() puts the
  // saved choice back (the picker's live preview, Escape reverts).
  API.preview = (id) => {
    if (!byId[id]) return;
    document.documentElement.setAttribute("data-theme", id);
    const m = document.querySelector('meta[name="theme-color"]');
    if (m) m.setAttribute("content", byId[id].vars.bg);
  };
  // counterpart: the same family in the other mode ("" when it has none), for
  // the header's light/dark toggle.
  const PAIRS = {
    dark: "light", light: "dark", "gruvbox-dark": "gruvbox-light", "gruvbox-light": "gruvbox-dark",
    "tokyonight-night": "tokyonight-day", "tokyonight-storm": "tokyonight-day", "tokyonight-day": "tokyonight-night",
    "catppuccin-mocha": "catppuccin-latte", "catppuccin-latte": "catppuccin-mocha",
    "solarized-dark": "solarized-light", "solarized-light": "solarized-dark",
  };
  API.counterpart = (id) => PAIRS[id] || "";
  API.set = (choice) => { ls.set(API.KEY, choice); API.apply(); };
  API.setSystem = (mode, id) => { ls.set(mode === "light" ? API.KEY_LIGHT : API.KEY_DARK, id); API.apply(); };

  // The Font setting: localStorage "comebye.font" holds "" (system sans, the default) or one of FONTS' ids;
  // fontApply puts it on <html data-font> before first paint, like the theme. A stored value this build does
  // not know is ignored, so the default shows.
  API.FONTS = [
    { id: "", name: "System sans", sub: "default" },
    { id: "serif-heads", name: "Serif headings", sub: "big headings only" },
    { id: "serif-reading", name: "Serif reading", sub: "briefing, To do, Markdown" },
    { id: "geist", name: "Geist Sans + Mono", sub: "UI and code" },
  ];
  API.KEY_FONT = "comebye.font";
  API.fontChoice = () => {
    let v = "";
    try { v = ls.get(API.KEY_FONT) || ""; } catch (e) { /* no storage: the default */ }
    return API.FONTS.some((f) => f.id === v) ? v : "";
  };
  API.fontApply = () => {
    const v = API.fontChoice(), el = document.documentElement;
    if (v) el.setAttribute("data-font", v); else el.removeAttribute("data-font");
    return v;
  };
  API.fontSet = (id) => { try { ls.set(API.KEY_FONT, id); } catch (e) { /* not saved */ } API.fontApply(); };
  API.fontApply();
  API.apply();
  if (mq && mq.addEventListener) mq.addEventListener("change", () => { if (API.choice() === "system") API.apply(); });
  root.addEventListener("storage", (e) => { if (e.key && e.key.indexOf(API.KEY) === 0) API.apply(); if (e.key === API.KEY_FONT) API.fontApply(); });
})(typeof window !== "undefined" ? window : globalThis);
