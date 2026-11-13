package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The publish-time layout check. When headless Chrome is on the machine,
// `comebye publish` renders an HTML page in two iframes, 390 and 1280 px wide,
// and warns about sideways scroll and clipped text. It is advice: it never
// blocks a publish, and it is skipped quietly when there is no Chrome, the
// render fails or it takes too long. COMEBYE_NO_LAYOUT_CHECK=1 turns it off;
// COMEBYE_CHROME names the browser.

const layoutTimeout = 10 * time.Second

var layoutWidths = []int{390, 1280}

type layoutIssue struct {
	Width int    `json:"width"`
	Kind  string `json:"kind"` // "scroll" or "clipped"
	What  string `json:"what"`
	Over  int    `json:"over"`
}

// layoutRender renders host (an HTML document) in headless Chrome and returns
// the DOM it ends with. Tests replace it.
var layoutRender = func(ctx context.Context, chrome, host string) (string, error) {
	dir, err := os.MkdirTemp("", "comebye-layout-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	f := filepath.Join(dir, "host.html")
	if err := os.WriteFile(f, []byte(host), 0o600); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, chrome, "--headless", "--no-sandbox", "--disable-gpu", "--no-first-run",
		"--hide-scrollbars", "--user-data-dir="+filepath.Join(dir, "profile"),
		"--virtual-time-budget=4000", "--dump-dom", "file://"+f)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// findChrome returns the browser to use, or "" when there is none.
func findChrome(e *Env) string {
	if p := e.Getenv("COMEBYE_CHROME"); p != "" {
		return p
	}
	for _, dir := range filepath.SplitList(e.Getenv("PATH")) {
		for _, n := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
			p := filepath.Join(dir, n)
			if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
				return p
			}
		}
	}
	return ""
}

const layoutProbe = `(function(){
var W=%s,res=[],done=0;
function look(fr,w){
 var d=fr.contentDocument,v=fr.contentWindow,out=[];
 var de=d.documentElement;
 if(de.scrollWidth>w+1)out.push({width:w,kind:"scroll",what:"the page",over:de.scrollWidth-w});
 var els=d.body.querySelectorAll("*"),n=0;
 for(var i=0;i<els.length&&n<5;i++){
  var el=els[i],cs=v.getComputedStyle(el);
  if(!/^(hidden|clip)$/.test(cs.overflowX))continue;
  if(cs.textOverflow==="ellipsis"||cs.display==="none")continue;
  if(el.clientWidth<1||el.scrollWidth<=el.clientWidth+1)continue;
  if(!(el.textContent||"").trim())continue;
  var t=el.tagName.toLowerCase();if(el.id)t+="#"+el.id;else if(el.className&&typeof el.className==="string")t+="."+el.className.trim().split(/\s+/)[0];
  out.push({width:w,kind:"clipped",what:t+": "+(el.textContent||"").trim().replace(/\s+/g," ").slice(0,40),over:el.scrollWidth-el.clientWidth});n++;
 }
 return out;
}
function fin(){var all=[];W.forEach(function(w){var fr=document.getElementById("f"+w);try{all=all.concat(look(fr,w))}catch(x){}});
 var p=document.createElement("pre");p.id="cbout";p.textContent=JSON.stringify(all);document.body.appendChild(p);}
W.forEach(function(w){document.getElementById("f"+w).addEventListener("load",function(){if(++done===W.length)setTimeout(fin,500)})});
})();`

func layoutHost(page string) string {
	var b strings.Builder
	ws, _ := json.Marshal(layoutWidths)
	b.WriteString("<!doctype html><meta charset=utf-8><body style=\"margin:0\">")
	for _, w := range layoutWidths {
		fmt.Fprintf(&b, "<iframe id=\"f%d\" style=\"width:%dpx;height:800px;border:0;display:block\" srcdoc=\"%s\"></iframe>", w, w, html.EscapeString(page))
	}
	b.WriteString("<script>" + fmt.Sprintf(layoutProbe, ws) + "</script>")
	return b.String()
}

var cbOutRE = regexp.MustCompile(`(?s)<pre id="cbout">(.*?)</pre>`)

func parseLayoutDOM(dom string) ([]layoutIssue, bool) {
	m := cbOutRE.FindStringSubmatch(dom)
	if m == nil {
		return nil, false
	}
	var issues []layoutIssue
	if json.Unmarshal([]byte(html.UnescapeString(m[1])), &issues) != nil {
		return nil, false
	}
	return issues, true
}

// layoutCheck returns the warning lines for an HTML page; none when all is
// well or when the check cannot run.
func layoutCheck(e *Env, page string) []string {
	if e.Getenv("COMEBYE_NO_LAYOUT_CHECK") != "" {
		return nil
	}
	chrome := findChrome(e)
	if chrome == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), layoutTimeout)
	defer cancel()
	dom, err := layoutRender(ctx, chrome, layoutHost(page))
	if err != nil {
		return nil
	}
	issues, ok := parseLayoutDOM(dom)
	if !ok {
		return nil
	}
	var lines []string
	for _, is := range issues {
		switch is.Kind {
		case "scroll":
			lines = append(lines, fmt.Sprintf("Layout: at %d px the page scrolls sideways (%d px too wide).", is.Width, is.Over))
		case "clipped":
			lines = append(lines, fmt.Sprintf("Layout: at %d px text is clipped in %s (%d px hidden).", is.Width, is.What, is.Over))
		}
	}
	return lines
}
