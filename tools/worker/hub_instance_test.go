package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHubSetInstance(t *testing.T) {
	var method, path, body string
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, path, body = r.Method, r.URL.Path, string(b)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	h := &hubClient{base: srv.URL, http: srv.Client()}
	if err := h.SetInstance(context.Background(), "w1", "i-0abc123"); err != nil {
		t.Fatal(err)
	}
	if method != "PUT" || path != "/api/peers/w1/instance" || body != `{"instance":"i-0abc123"}` {
		t.Fatalf("%s %s %s", method, path, body)
	}
	status = 409
	if err := h.SetInstance(context.Background(), "w1", "i-1"); err == nil {
		t.Fatal("a refusal must be an error")
	}
}
