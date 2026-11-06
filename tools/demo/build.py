#!/usr/bin/env python3
"""Builds the Comebye tour page: the real web/ app, inlined into one file, with
the fake backend before app.js and the tour after it.

    tools/demo/build.py OUT.html [--wrap]

The app's sources are not edited. The only changes are mechanical: its <head>
links (manifest, icons) go, because the page has no files beside it, and two
strings that would upset inline <script> parsing are escaped. --wrap adds the
skeleton the Artifact host adds at publish time, for testing from a local file.
"""
import pathlib, re, sys

here = pathlib.Path(__file__).resolve().parent
web = here.parent.parent / "web"
out = pathlib.Path(sys.argv[1])
wrap = "--wrap" in sys.argv

html = (web / "index.html").read_text()
style = re.search(r"<style>(.*?)</style>", html, re.S).group(1)
theme = re.search(r"<script>(.*?)</script>", html, re.S).group(1)
body = re.search(r"<body>(.*?)<script src=", html, re.S).group(1)

def inline(js):
    # An HTML parser reads <!-- and <script inside a script element specially.
    js = js.replace("<!--", "<\\!--").replace("<script", "<\\x73cript").replace("</script", "<\\/script")
    return f"<script>{js}</script>"

def read(p):
    return pathlib.Path(p).read_text()

page = f"""<title>Comebye Tour</title>
<meta name="theme-color" content="#101218" media="(prefers-color-scheme: dark)">
<meta name="theme-color" content="#f3f4f7" media="(prefers-color-scheme: light)">
<script>{theme}</script>
<style>{style}</style>
<style>{read(here / "tour.css")}</style>
{body}
{inline(read(web / "vendor/marked.umd.js"))}
{inline(read(web / "vendor/purify.min.js"))}
{inline(read(here / "mock.js"))}
{inline(read(web / "app.js"))}
{inline(read(here / "tour.js"))}
"""
if wrap:
    page = ('<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">'
            "<style>:root{color-scheme:light;padding-top:env(safe-area-inset-top,0px);padding-bottom:env(safe-area-inset-bottom,0px)}"
            "body{margin:0;font:14px system-ui}img{max-width:100%}[hidden]{display:none!important}</style></head><body>" + page + "</body></html>")
out.write_text(page)
print(out, len(page), "bytes")
