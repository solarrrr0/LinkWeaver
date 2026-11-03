package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func exampleConfig() Config {
	c := defaults()
	c.FixedExit = map[string]any{"type": "socks5", "server": "exit.invalid", "port": 1080, "username": "secret-user", "password": "never-return-this"}
	c.Nodes = []Node{{ID: "one", Label: "One", Proxy: map[string]any{"type": "vless", "server": "relay.invalid", "port": 443, "uuid": "credential", "reality-opts": map[string]any{"public-key": "keep-me", "short-id": "abcd"}}}}
	return c
}
func TestGeneratedConfigPreservesProtocolAndFailsClosed(t *testing.T) {
	c := exampleConfig()
	cfg := buildCoreConfig(c, "controller-token")
	ps := cfg["proxies"].([]any)
	relay := ps[0].(map[string]any)
	chain := ps[1].(map[string]any)
	if relay["reality-opts"].(map[string]any)["public-key"] != "keep-me" {
		t.Fatal("protocol options lost")
	}
	if chain["dialer-proxy"] != "relay-one" {
		t.Fatal("wrong chain direction")
	}
	if c.Nodes[0].Proxy["name"] != nil || c.FixedExit["dialer-proxy"] != nil {
		t.Fatal("mutated imported config")
	}
	for _, g := range cfg["proxy-groups"].([]any) {
		m := g.(map[string]any)
		if m["default-selected"] != "REJECT" {
			t.Fatal("startup not closed")
		}
		for _, n := range m["proxies"].([]string) {
			if n == "DIRECT" {
				t.Fatal("direct fallback forbidden")
			}
		}
	}
	if cfg["unified-delay"] != false {
		t.Fatal("must include handshakes")
	}
	if len(cfg["listeners"].([]any)) != 2 {
		t.Fatal("listener count must not grow with candidates")
	}
}
func TestValidation(t *testing.T) {
	c := exampleConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.VerifyPort = c.ProxyPort
	if c.Validate() == nil {
		t.Fatal("port conflict not rejected")
	}
	c = exampleConfig()
	c.Targets = []string{"https://user:pass@example.com"}
	if c.Validate() == nil {
		t.Fatal("credential target not rejected")
	}
}
func TestAtomicReplacement(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	c := exampleConfig()
	if err := saveConfig(p, c); err != nil {
		t.Fatal(err)
	}
	c.Name = "Updated"
	if err := saveConfig(p, c); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig(p)
	if err != nil || got.Name != "Updated" {
		t.Fatal("cannot replace config")
	}
	b, _ := json.Marshal(got)
	if len(b) < 100 {
		t.Fatal("truncated")
	}
}
