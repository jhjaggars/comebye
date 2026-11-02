// Shared by drive.mjs (the scripted regression run) and shot.mjs (the dev
// loop's screenshot CLI): the headless Chrome setup and the mocked routes
// for the launch sheet, so neither depends on which agents the host has.
import { chromium } from "playwright-core";

export const CHROME = process.env.CHROME || "/usr/bin/google-chrome";

// Launches headless Chrome with a phone-style context (390x844 by default;
// width > 500 makes a desktop one: no touch, no mobile emulation).
export async function launch({ width = 390, height = 844, dark } = {}) {
  const browser = await chromium.launch({ executablePath: CHROME, headless: true, args: ["--no-sandbox"] });
  const mobile = width <= 500;
  const ctx = await browser.newContext({
    viewport: { width, height }, deviceScaleFactor: mobile ? 2 : 1, isMobile: mobile, hasTouch: mobile, bypassCSP: true,
    ...(mobile ? { userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1" } : {}),
    ...(dark === undefined ? {} : { colorScheme: dark ? "dark" : "light" }),
  });
  return { browser, ctx, page: await ctx.newPage() };
}

export const CATALOGS = {
  pi: { kind: "pi", source: "pi --list-models", exact: true, default: "homelab/thinking", default_thinking: "minimal",
    thinking_levels: ["off", "minimal", "low", "medium", "high", "xhigh", "max"],
    models: [
      { id: "homelab/fast", provider: "homelab", name: "fast" },
      { id: "homelab/thinking", provider: "homelab", name: "thinking", thinking: true },
      { id: "openai-codex/gpt-5.5", provider: "openai-codex", name: "gpt-5.5", thinking: true },
      { id: "openai-codex/gpt-5.6-luna", provider: "openai-codex", name: "gpt-5.6-luna", thinking: true },
    ] },
  claude: { kind: "claude", source: "curated", models: [
    { id: "opus", provider: "Aliases", note: "newest Opus" }, { id: "sonnet", provider: "Aliases", note: "newest Sonnet" },
    { id: "claude-opus-5-5", provider: "Full ids" } ] },
  codex: { kind: "codex", source: "none", models: [] },
};

// Mocks the launch sheet's routes. hooks.onModels(path) and
// hooks.onLaunch(body) see what the app asked for.
export async function mockLaunchRoutes(page, base, hooks = {}) {
  await page.route(/\/api\/launch\/options/, (r) => r.fulfill({ json: {
    enabled: true, kinds: ["pi", "claude", "codex", "cursor"], default_kind: "pi", model_kinds: ["pi", "claude", "codex"],
    roots: [{ name: "~/code", path: "/home/u/code", label: "~/code", git: false }],
    recent: [{ name: "proj", path: "/home/u/code/proj", label: "~/code/proj", git: true }] } }));
  await page.route(/\/api\/launch\/dirs/, (r) => r.fulfill({ json: { path: "/home/u/code", label: "~/code", dirs: [] } }));
  await page.route(/\/api\/models/, (r) => {
    hooks.onModels?.(r.request().url().replace(base, ""));
    const kind = new URL(r.request().url()).searchParams.get("kind");
    r.fulfill({ json: { host: "uitest", catalogs: CATALOGS[kind] ? [CATALOGS[kind]] : [] } });
  });
  await page.route(/\/api\/launch$/, (r) => {
    if (r.request().method() !== "POST") return r.continue();
    hooks.onLaunch?.(JSON.parse(r.request().postData() || "{}"));
    r.fulfill({ json: { agent: { pane_id: "w1:p1", name: "alpha" }, workspace_id: "w9" } });
  });
}
