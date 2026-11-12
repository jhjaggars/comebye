package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/web"
)

// The Font setting loads Geist from the app's own origin: the CSP must allow it (font-src 'self') and the
// embedded woff2 files must be served as fonts.
func TestFontsServedAndAllowedByCSP(t *testing.T) {
	if !strings.Contains(appCSP(""), "font-src 'self'") {
		t.Fatalf("appCSP lacks font-src 'self': %s", appCSP(""))
	}
	h, err := newStaticHandler(web.FS)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"GeistSerif-Regular", "GeistSans-Variable", "GeistMono-Variable"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vendor/fonts/"+f+".woff2", nil))
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "font/woff2" || rec.Body.Len() < 10000 {
			t.Errorf("%s: %d %q %d bytes", f, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
		}
	}
}
