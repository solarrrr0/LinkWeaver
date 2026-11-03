package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func clashFixture(t *testing.T) *Server {
	t.Helper()
	c := defaults()
	p := testProfile("one", 32020)
	p.Name = "工作 & 出口 / 一"
	q := testProfile("two", 32030)
	q.Name = p.Name
	c.Profiles = []ExitProfile{p, q}
	w, err := newWorkspace(t.TempDir(), c)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range c.Profiles {
		e, _ := w.engine(p.ID)
		e.state = "running"
		e.current = "relay"
		e.observedIP = "192.0.2.1"
	}
	return &Server{workspace: w, token: "session-secret"}
}
func exportInfo(t *testing.T, s *Server, query string) map[string]any {
	t.Helper()
	rec := request(s, http.MethodGet, "/api/clash/export"+query, "127.0.0.1:28180", s.token, "", "")
	if rec.Code != 200 {
		t.Fatal("export metadata failed", rec.Code)
	}
	var info map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	return info
}
func TestClashSubscriptionLocalEntrancesAndStableKey(t *testing.T) {
	s := clashFixture(t)
	info := exportInfo(t, s, "")
	if info["entranceCount"] != float64(2) {
		t.Fatal("missing ready entrances")
	}
	link, err := url.Parse(info["subscriptionURL"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if link.Host != "127.0.0.1:28180" || len(link.Query().Get("key")) != 64 {
		t.Fatal("invalid subscription address")
	}
	uri, _ := url.Parse(info["installURI"].(string))
	if uri.Scheme != "clash" || uri.Host != "install-config" || uri.Query().Get("url") != link.String() {
		t.Fatal("Clash URI escaping lost subscription URL")
	}
	rec := request(s, "GET", link.RequestURI(), link.Host, "", "", "")
	if rec.Code != 200 {
		t.Fatal("native client could not fetch subscription", rec.Code)
	}
	for _, private := range []string{"fixture-secret", "exit.invalid", "password:", "uuid:", "session-secret", link.Query().Get("key"), "DIRECT"} {
		if strings.Contains(rec.Body.String(), private) {
			t.Fatal("subscription leaked upstream data or direct fallback")
		}
	}
	var doc clashDocument
	if err := yaml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Proxies) != 2 || doc.Proxies[0].Name == doc.Proxies[1].Name || len(doc.Groups) != 1 || len(doc.Rules) != 1 || doc.Rules[0] != "MATCH,链跃出口" {
		t.Fatal("invalid proxy groups or duplicate labels")
	}
	for _, p := range doc.Proxies {
		if p.Server != "127.0.0.1" || p.Type != "socks5" || p.UDP {
			t.Fatal("export bypassed the local chain manager")
		}
	}
	again := &Server{workspace: s.workspace, token: "new-session"}
	after := exportInfo(t, again, "")
	if after["subscriptionURL"] != info["subscriptionURL"] {
		t.Fatal("subscription changed across application restart")
	}
	head := request(again, "HEAD", link.RequestURI(), link.Host, "", "", "")
	if head.Code != 200 || head.Body.Len() != 0 {
		t.Fatal("invalid HEAD response")
	}
	selected := exportInfo(t, s, "?profile=one")
	selectedURL, _ := url.Parse(selected["subscriptionURL"].(string))
	rec = request(s, "GET", selectedURL.RequestURI(), link.Host, "", "", "")
	if err = yaml.Unmarshal(rec.Body.Bytes(), &doc); err != nil || len(doc.Proxies) != 1 || doc.Proxies[0].Port != 32020 {
		t.Fatal("per-profile export included another profile")
	}
}

func TestClashSubscriptionGuardsAndReadiness(t *testing.T) {
	s := clashFixture(t)
	info := exportInfo(t, s, "")
	link, _ := url.Parse(info["subscriptionURL"].(string))
	for _, tc := range []struct {
		path, host, origin, method string
		code                       int
	}{
		{"/clash/subscription.yaml", link.Host, "", "GET", 403},
		{"/clash/subscription.yaml?key=wrong", link.Host, "", "GET", 403},
		{link.RequestURI(), "evil.invalid:28180", "", "GET", 403},
		{link.RequestURI(), link.Host, "https://evil.invalid", "GET", 403},
		{link.RequestURI(), link.Host, "", "POST", 405},
	} {
		if got := request(s, tc.method, tc.path, tc.host, "", tc.origin, ""); got.Code != tc.code {
			t.Fatalf("subscription guard: got %d, want %d", got.Code, tc.code)
		}
	}
	e, _ := s.workspace.engine("two")
	e.state = "stopped"
	if exportInfo(t, s, "")["entranceCount"] != float64(1) {
		t.Fatal("exported stopped entrance")
	}
	if rec := request(s, "GET", "/api/clash/export?profile=two", link.Host, s.token, "", ""); rec.Code != 400 {
		t.Fatal("exported stopped profile")
	}
	e, _ = s.workspace.engine("one")
	e.observedIP = ""
	if rec := request(s, "GET", link.RequestURI(), link.Host, "", "", ""); rec.Code != 409 {
		t.Fatal("exported unverified listener")
	}
	if rec := request(s, "GET", "/api/clash/export?profile=missing", link.Host, s.token, "", ""); rec.Code != 400 {
		t.Fatal("accepted missing profile")
	}
}

func TestClashInstallOnlyOnAuthenticatedRequest(t *testing.T) {
	s := clashFixture(t)
	opened := ""
	s.openClashURI = func(uri string) error { opened = uri; return nil }
	if rec := request(s, "POST", "/api/clash/install", "127.0.0.1:28180", "", "", `{"profile":"one"}`); rec.Code != 403 || opened != "" {
		t.Fatal("unauthenticated launch")
	}
	if rec := request(s, "GET", "/api/clash/install", "127.0.0.1:28180", s.token, "", ""); rec.Code != 405 || opened != "" {
		t.Fatal("GET launched an application")
	}
	rec := request(s, "POST", "/api/clash/install", "127.0.0.1:28180", s.token, "", `{"profile":"one"}`)
	if rec.Code != 200 || opened == "" || !strings.Contains(rec.Body.String(), `"requested":true`) {
		t.Fatal("install request failed")
	}
	uri, _ := url.Parse(opened)
	link, _ := url.Parse(uri.Query().Get("url"))
	if link.Query().Get("profile") != "one" {
		t.Fatal("installer lost selected profile")
	}
	s.openClashURI = func(string) error { return errors.New("fixture launch failure") }
	if rec := request(s, "POST", "/api/clash/install", "127.0.0.1:28180", s.token, "", `{}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "fixture launch failure") {
		t.Fatal("launch failure was reported as imported")
	}
}
