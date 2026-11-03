package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"
)

func fixtureWorkspace(t *testing.T, withCore bool) *Workspace {
	t.Helper()
	root := t.TempDir()
	if withCore {
		if err := os.MkdirAll(filepath.Join(root, "bin"), 0700); err != nil {
			t.Fatal(err)
		}
		core, err := os.ReadFile("bin/mihomo.exe")
		if os.IsNotExist(err) {
			core, err = os.ReadFile("../bin/mihomo.exe")
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(root, "bin", "mihomo.exe"), core, 0700); err != nil {
			t.Fatal(err)
		}
	}
	w, err := newWorkspace(root, defaults())
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func testProfile(id string, port int) ExitProfile {
	return ExitProfile{ID: id, Name: "Fixture " + id, ProxyPort: port, VerifyPort: port + 1, ControllerPort: port + 2, FixedExit: map[string]any{"type": "socks5", "server": "exit.invalid", "port": 1080, "password": "fixture-secret"}}
}

func TestWorkspaceMigrationAndFiltering(t *testing.T) {
	c := exampleConfig()
	c.Version = 1
	w, err := newWorkspace(t.TempDir(), c)
	if err != nil {
		t.Fatal(err)
	}
	got := w.snapshot()
	if len(got.Profiles) != 1 || got.Profiles[0].ProxyPort != c.ProxyPort || len(got.Nodes) != len(c.Nodes) || got.FixedExit != nil {
		t.Fatal("migration lost settings")
	}
	got.Sources = append(got.Sources, RelaySource{ID: "off", Name: "off", Enabled: false, Priority: 100})
	n := got.Nodes[0]
	n.ID = "off-node"
	n.SourceID = "off"
	got.Nodes = append(got.Nodes, n)
	p := got.Profiles[0]
	if len(profileConfig(got, p).Nodes) != len(c.Nodes) {
		t.Fatal("disabled source leaked")
	}
	got.Nodes[0].Disabled = true
	if len(profileConfig(got, p).Nodes) != len(c.Nodes)-1 {
		t.Fatal("disabled node leaked")
	}
	p.AllowedSources = []string{"deleted"}
	if len(profileConfig(got, p).Nodes) != 0 {
		t.Fatal("missing source broadened scope")
	}
	p = got.Profiles[0]
	p.ID = "duplicate"
	got.Profiles = append(got.Profiles, p)
	if validateWorkspace(got) == nil {
		t.Fatal("accepted overlapping ports")
	}
}

func TestCredentialMergeAndRedaction(t *testing.T) {
	old := map[string]any{"type": "socks5", "password": "old-secret", "username": "old-user", "nested": map[string]any{"private-key": "nested-secret", "keep": true}}
	kept := mergeProxy(old, map[string]any{"password": "", "nested": map[string]any{"keep": false}})
	if kept["password"] != "old-secret" || kept["nested"].(map[string]any)["private-key"] != "nested-secret" {
		t.Fatal("blank editing removed credentials")
	}
	for i := 0; i < 50; i++ {
		fresh := mergeProxy(old, map[string]any{"_clearAuth": true, "password": "new-secret"})
		if fresh["password"] != "new-secret" || fresh["username"] != nil || fresh["nested"].(map[string]any)["private-key"] != nil {
			t.Fatal("clear auth order is unstable")
		}
	}
	b, _ := json.Marshal(safeProxy(old))
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "old-user") {
		t.Fatal("nested credentials leaked")
	}
	changed := mergeProxy(old, map[string]any{"type": "vless", "uuid": "new-id"})
	if changed["password"] != nil || changed["nested"] != nil {
		t.Fatal("old protocol fields retained")
	}
}

func TestSubscriptionRefreshCRUDAndRollback(t *testing.T) {
	failFetch := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failFetch {
			w.WriteHeader(503)
			return
		}
		if r.Header.Get("User-Agent") != "Clash.Meta" {
			t.Error("missing user agent")
		}
		_, _ = w.Write([]byte("proxies:\n- name: Relay\n  type: socks5\n  server: relay.invalid\n  port: 1080\n  password: fixture-relay-secret\n"))
	}))
	defer upstream.Close()
	w := fixtureWorkspace(t, true)
	if err := w.saveSource(RelaySource{Name: "test", URL: upstream.URL, Enabled: true, Priority: 100}); err != nil {
		t.Fatal(err)
	}
	source := w.snapshot().Sources[0]
	if err := w.refreshSource(source.ID); err != nil {
		t.Fatal(err)
	}
	c := w.snapshot()
	if len(c.Nodes) != 1 {
		t.Fatal("missing imported node")
	}
	source.Priority = 50
	if err := w.saveSource(source); err != nil {
		t.Fatal(err)
	}
	if w.snapshot().Nodes[0].Priority != 50 {
		t.Fatal("source priority not inherited")
	}
	n := w.snapshot().Nodes[0]
	n.Priority = 30
	n.Disabled = true
	n.Proxy = map[string]any{}
	if err := w.saveNode(n); err != nil {
		t.Fatal(err)
	}
	if err := w.refreshSource(source.ID); err != nil {
		t.Fatal(err)
	}
	n = w.snapshot().Nodes[0]
	if !n.Disabled || n.Priority != 30 || n.Proxy["password"] != "fixture-relay-secret" {
		t.Fatal("refresh lost node overrides")
	}
	failFetch = true
	if w.refreshSource(source.ID) == nil {
		t.Fatal("accepted upstream failure")
	}
	if len(w.snapshot().Nodes) != 1 || w.snapshot().Sources[0].LastError == "" {
		t.Fatal("failed refresh destroyed cached nodes")
	}
	source.Enabled = false
	if err := w.saveSource(source); err != nil {
		t.Fatal(err)
	}
	if w.refreshSource(source.ID) == nil {
		t.Fatal("refreshed disabled source")
	}
	// Profile CRUD checks protocol validation against the actual packaged core.
	n.Disabled = false
	if err := w.saveNode(n); err != nil {
		t.Fatal(err)
	}
	source.Enabled = true
	if err := w.saveSource(source); err != nil {
		t.Fatal(err)
	}
	p := testProfile("", 32020)
	if err := w.saveProfile(p); err != nil {
		t.Fatal(err)
	}
	c = w.snapshot()
	p = c.Profiles[0]
	first, _ := w.engine(p.ID)
	p.Name = "Renamed"
	p.FixedExit = map[string]any{}
	if err := w.saveProfile(p); err != nil {
		t.Fatal(err)
	}
	same, _ := w.engine(p.ID)
	if first != same {
		t.Fatal("rename recreated engine")
	}
	if w.snapshot().Profiles[0].FixedExit["password"] != "fixture-secret" {
		t.Fatal("edit erased exit credentials")
	}
	second := testProfile("", 32030)
	if err := w.saveProfile(second); err != nil {
		t.Fatal(err)
	}
	other := w.snapshot().Profiles[1]
	otherEngine, _ := w.engine(other.ID)
	p.FixedExit = map[string]any{"port": 1081}
	if err := w.saveProfile(p); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := w.engine(other.ID)
	if otherEngine != unchanged {
		t.Fatal("unrelated exit restarted")
	}
	p.FixedExit = map[string]any{"type": "unsupported-protocol"}
	if w.saveProfile(p) == nil {
		t.Fatal("bad protocol accepted")
	}
	if w.snapshot().Profiles[0].FixedExit["type"] != "socks5" {
		t.Fatal("failed validation changed persisted configuration")
	}
	s := &Server{workspace: w, token: "t"}
	for _, path := range []string{"/api/workspace", "/api/profiles/detail?id=" + p.ID, "/api/relays/detail?id=" + n.ID} {
		rec := request(s, "GET", path, "127.0.0.1:28180", "t", "", "")
		if rec.Code != 200 {
			t.Fatal(rec.Code)
		}
		if strings.Contains(rec.Body.String(), "fixture-secret") || strings.Contains(rec.Body.String(), "fixture-relay-secret") {
			t.Fatal("API credential leak")
		}
	}
	c = w.snapshot()
	c.Profiles[0].AllowedSources = []string{source.ID}
	if err := w.commit(c); err != nil {
		t.Fatal(err)
	}
	if err := w.deleteSource(source.ID); err != nil {
		t.Fatal(err)
	}
	c = w.snapshot()
	if len(c.Nodes) != 0 || len(profileConfig(c, c.Profiles[0]).Nodes) != 0 || len(c.Profiles[0].AllowedSources) == 0 {
		t.Fatal("deleting selected source broadened pool")
	}
	if err := w.deleteProfile(p.ID); err != nil {
		t.Fatal(err)
	}
	if len(w.snapshot().Profiles) != 1 {
		t.Fatal("profile delete failed")
	}
}

func TestProxyParsers(t *testing.T) {
	tests := []struct{ input, typ string }{
		{"https://user:p%40ss@[2001:db8::1]:443#Secure", "http"},
		{"vless://example-uuid@example.invalid:443?security=reality&pbk=public&sid=ab&type=ws&path=%2Fws&sni=sni.invalid", "vless"},
		{"ss://" + base64.RawStdEncoding.EncodeToString([]byte("aes-128-gcm:p@ss")) + "@relay.invalid:8388#SS", "ss"},
		{base64.StdEncoding.EncodeToString([]byte("trojan://password@relay.invalid:443#T")), "trojan"},
		{"proxies:\n- {name: Demo, type: socks5, server: relay.invalid, port: 1080, password: abc}", "socks5"},
		{"VMESS://" + base64.StdEncoding.EncodeToString([]byte(`{"v":"2","ps":"vm","add":"relay.invalid","port":"443","id":"uuid","net":"grpc","path":"svc","tls":"tls"}`)), "vmess"},
	}
	for _, tt := range tests {
		nodes, err := parseProxyText(tt.input)
		if err != nil {
			t.Fatal(err)
		}
		p := nodes[0]
		if p["type"] != tt.typ {
			t.Fatal("protocol mismatch")
		}
		if tt.typ == "http" && (p["tls"] != true || p["password"] != "p@ss" || p["server"] != "2001:db8::1") {
			t.Fatal("HTTPS or IPv6 parsed incorrectly")
		}
		if tt.typ == "vless" && (p["reality-opts"] == nil || p["ws-opts"] == nil || p["servername"] != "sni.invalid") {
			t.Fatal("lost Reality or transport options")
		}
		if tt.typ == "vmess" && p["grpc-opts"] == nil {
			t.Fatal("lost gRPC options")
		}
	}
	for _, bad := range []string{"socks5://host:0", "nonsense", "proxies: []", "ss://YWVzLTEyOC1nY206cA@host:443?plugin=obfs-local", "{type: socks5, server: host, port: 1.5}"} {
		if _, err := parseProxyText(bad); err == nil {
			t.Fatal("accepted malformed input")
		}
	}
}

func TestRestoreEmptySourcesAndTrayABI(t *testing.T) {
	w := fixtureWorkspace(t, false)
	s := &Server{workspace: w, token: "t"}
	c := defaults()
	c.Sources = []RelaySource{}
	c.Nodes = exampleConfig().Nodes
	raw, _ := json.Marshal(c)
	body, _ := json.Marshal(map[string]string{"content": string(raw)})
	rec := request(s, "POST", "/api/import", "127.0.0.1:28180", "t", "", string(body))
	if rec.Code != 200 {
		t.Fatal("restore failed", rec.Code)
	}
	if len(w.snapshot().Sources) != 1 {
		t.Fatal("missing source after backup restore")
	}
	if unsafe.Sizeof(uintptr(0)) == 8 && (unsafe.Sizeof(notifyIcon{}) != 976 || unsafe.Sizeof(windowClass{}) != 80 || unsafe.Sizeof(winMessage{}) != 48) {
		t.Fatal("Windows x64 tray structure layout mismatch")
	}
}
