package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func request(s *Server, method, path, host, token, origin, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(body))
	r.Host = host
	r.Header.Set("X-LinkWeaver-Token", token)
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}
func TestLocalAPIGuardsAndRedaction(t *testing.T) {
	s := &Server{engine: newEngine(t.TempDir(), exampleConfig()), token: "test-token", shutdown: make(chan struct{}, 1)}
	for _, x := range []struct{ host, token, origin string }{{"attacker.test:28180", "test-token", ""}, {"127.0.0.1:28180", "", ""}, {"127.0.0.1:28180", "test-token", "https://attacker.test"}} {
		if w := request(s, "GET", "/api/status", x.host, x.token, x.origin, ""); w.Code != 403 {
			t.Fatalf("guard failed: %d", w.Code)
		}
	}
	w := request(s, "GET", "/api/status", "127.0.0.1:28180", "test-token", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	for _, secret := range []string{"never-return-this", "secret-user", "relay.invalid", "exit.invalid", "credential", "controller-token"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("sensitive config escaped through status")
		}
	}
	if w := request(s, "GET", "/api/start", "127.0.0.1:28180", "test-token", "", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatal("GET mutated state")
	}
}
func TestImportRejectedWhenRunning(t *testing.T) {
	e := newEngine(t.TempDir(), exampleConfig())
	e.state = "running"
	s := &Server{engine: e, token: "t"}
	body := `{"content":"nodes: [{id: one, label: One, proxy: {type: socks5, server: relay.invalid, port: 1080}}]\nfixedExit: {type: socks5, server: exit.invalid, port: 1080}"}`
	w := request(s, "POST", "/api/import", "127.0.0.1:28180", "t", "", body)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "停止") {
		t.Fatal("running config may be overwritten", w.Body.String())
	}
}
