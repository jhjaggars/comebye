# The Comebye demo

A clickable tour of the desktop app, and the screenshots made from it, for
people who want to see Comebye without installing it. It's the real `web/` app
running against a fake backend in the page: nothing talks to a server, and every
agent, repo and transcript is invented. Keep it that way: never paste real agent
text, hostnames or logins into `mock.js`.

- `mock.js`: the fake backend, a `fetch` and `EventSource` that answer every `/api` route the app calls
- `tour.js`, `tour.css`: the intro panel and the guided tour
- `build.py`: inlines `web/` and the above into one HTML file
- `check.mjs`: clicks through the tour and the main interactions in headless Chrome
- `shots.mjs`: makes the screenshots, the walkthrough video and the gallery page

The app doesn't load any of this. A UI change only reaches the demo when you
rebuild and republish.

## Prerequisites

Python 3, Node, Google Chrome (or set `CHROME=/path/to/chrome`), and for the
shots `ffmpeg` and ImageMagick's `magick`. Install the browser driver once:

```sh
(cd tools/uitest && npm ci)
```

## Run the tour locally

```sh
python3 tools/demo/build.py /tmp/comebye-tour.html --wrap
python3 -m http.server -d /tmp 8000   # then open http://localhost:8000/comebye-tour.html
```

`--wrap` adds the page skeleton the Artifact host adds when publishing. The tour
needs a window at least 1100px wide; narrower, you get the phone layout.

## Check it

```sh
python3 tools/demo/build.py /tmp/comebye-tour.html --wrap
node tools/demo/check.mjs /tmp/comebye-tour.html /tmp/comebye-tour-shots
```

It exits non-zero on any failed step, console error or `/api` route the mock
doesn't answer. A new route in the app usually needs a new case in `route()` in `mock.js`.

## Regenerate the screenshots

```sh
node tools/demo/shots.mjs /tmp/comebye-shots            # everything
node tools/demo/shots.mjs /tmp/comebye-shots --only=shots   # or video, or gallery
```

The scenes, captions and walkthrough are all in `shots.mjs`. The output is a
`dark/` and `light/` PNG of each feature (desktop 1440×900 at 2x, phone 390×844
at 2x), `12-light-and-dark.png`, `video/` (MP4, WebM, GIF, poster) and `gallery.html`.

## Republish

The pages are Artifacts, published with Claude Code's Artifact tool, and only
their owner can update them. Both are one-file-plus-assets pages, so republishing keeps the URL:

- **Tour** (https://claude.ai/artifact/UTWy3VcH9Vyriu5FcdutEB): build without `--wrap`, and publish that one file to the URL:
  `python3 tools/demo/build.py /tmp/comebye-tour.html`
- **Gallery** (https://claude.ai/artifact/CT5UEDMUgX7hLUBzuvtrfm): publish `gallery.html` to the URL, with `root` set to the output folder and `dark/`, `light/`, `video/` (except the GIF, which the page doesn't use) and `12-light-and-dark.png` as `files`.

Each page links to the other: the URLs are constants at the top of `tour.js`
(`GALLERY`) and `shots.mjs` (`TOUR_URL`).
