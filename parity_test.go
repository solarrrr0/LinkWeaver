package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestSharedSourceRetirementAndDeletion(t *testing.T) {
	c := defaults()
	a := RelaySource{ID: "a", Name: "a", Enabled: true, Priority: 20}
	b := RelaySource{ID: "b", Name: "b", Enabled: true, Priority: 10}
	c.Sources = []RelaySource{a, b}
	proxy := map[string]any{"name": "sample", "type": "socks5", "server": "relay.invalid", "port": 1080}
	c = mergeSourceNodes(c, a, []map[string]any{proxy})
	id := c.Nodes[0].ID
	c.Nodes[0].Disabled = true
	c.Nodes[0].Priority = 7
	c = mergeSourceNodes(c, b, []map[string]any{proxy})
	if len(c.Nodes) != 1 || len(nodeSources(c.Nodes[0])) != 2 {
		t.Fatal("dedup lost membership")
	}
	c = mergeSourceNodes(c, a, nil)
	if c.Nodes[0].Retired || !slices.Contains(nodeSources(c.Nodes[0]), "b") {
		t.Fatal("refresh of one source removed shared node")
	}
	c = mergeSourceNodes(c, b, nil)
	if !c.Nodes[0].Retired {
		t.Fatal("removed node not retired")
	}
	c = mergeSourceNodes(c, a, []map[string]any{proxy})
	if c.Nodes[0].Retired || !c.Nodes[0].Disabled || c.Nodes[0].Priority != 7 || c.Nodes[0].ID != id {
		t.Fatal("refresh lost user preferences")
	}
	c.DeletedNodes = []string{proxyID(proxy)}
	c.Nodes = nil
	c = mergeSourceNodes(c, a, []map[string]any{proxy})
	if len(c.Nodes) != 0 {
		t.Fatal("deleted subscription node resurrected")
	}
}
func TestGroupAndProfileConstraints(t *testing.T) {
	c := defaults()
	c.Sources = []RelaySource{{ID: "a", Name: "a", Priority: 5, Enabled: true}, {ID: "b", Name: "b", Priority: 50, Enabled: true, Paused: true}}
	c.Nodes = []Node{{ID: "n", Label: "n", SourceIDs: []string{"a", "b"}, Priority: 100, Proxy: map[string]any{"type": "socks5", "server": "relay.invalid", "port": 1080}}}
	p := testProfile("a", 31000)
	p.AllowedSources = []string{"b"}
	got := profileConfig(c, p)
	if len(got.Nodes) != 1 || got.Nodes[0].GroupPriority != 50 || !got.Nodes[0].Paused {
		t.Fatal("source restriction leaked another source's priority")
	}
	p.RestrictNodes = true
	if len(profileConfig(c, p).Nodes) != 0 {
		t.Fatal("explicit empty allowlist broadened pool")
	}
	p.AllowedNodes = []string{"n"}
	c.PoolMode = "custom"
	c.RelayGroups = []RelayGroup{{ID: "g", Name: "g", Priority: 3, Paused: true, Members: []RelayGroupMember{{NodeID: "n", Priority: 9}}}}
	n := profileConfig(c, p).Nodes[0]
	if n.GroupPriority != 3 || n.Priority != 9 || !n.Paused {
		t.Fatal("custom group policy missing")
	}
	c.RelayGroups = append(c.RelayGroups, RelayGroup{ID: "h", Name: "h", Priority: 4, Members: []RelayGroupMember{{NodeID: "n", Priority: 1}}})
	if validateGroups(c) == nil {
		t.Fatal("duplicate group member accepted")
	}
}
func TestStoppedManualSelectionPersistence(t *testing.T) {
	w := fixtureWorkspace(t, false)
	c := w.snapshot()
	p := testProfile("p", 32000)
	c.Profiles = []ExitProfile{p}
	c.Sources = []RelaySource{{ID: "s", Name: "s", Enabled: true, Priority: 100}}
	c.Nodes = []Node{{ID: "n", Label: "n", SourceID: "s", Priority: 100, Proxy: map[string]any{"type": "socks5", "server": "relay.invalid", "port": 1080}}}
	var err error
	w, err = newWorkspace(w.root, c)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.setSelection("p", "n"); err != nil {
		t.Fatal(err)
	}
	saved, err := loadConfig(filepath.Join(w.root, "data", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	w2, err := newWorkspace(w.root, saved)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := w2.engine("p")
	if e.config.SelectionMode != "manual" || e.config.ManualRelayID != "n" {
		t.Fatal("manual preference lost on reload")
	}
	if w2.setSelection("p", "outside") == nil {
		t.Fatal("accepted outside candidate")
	}
	if err = w2.setSelection("p", ""); err != nil {
		t.Fatal(err)
	}
	if w2.snapshot().Profiles[0].SelectionMode != "automatic" {
		t.Fatal("automatic selection not saved")
	}
}
func TestProbeOnlyHasNoExitOrListeners(t *testing.T) {
	c := exampleConfig()
	c.FixedExit = nil
	c.ProbeOnly = true
	b, err := json.Marshal(buildCoreConfig(c, "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	if _, ok := doc["listeners"]; ok {
		t.Fatal("health probe opened listeners")
	}
	for _, raw := range doc["proxies"].([]any) {
		p := raw.(map[string]any)
		if _, ok := p["dialer-proxy"]; ok {
			t.Fatal("health probe requires fixed exit")
		}
	}
}

func TestPausedRouteCannotBeSelectedByEarlyAutomaticProbe(t *testing.T) {
	c := exampleConfig()
	c.Nodes[0].Paused = true
	e := newEngine(t.TempDir(), c)
	e.state = "running"
	if err := e.trySelect(context.Background(), c.Nodes[0].ID, "首次完整链路可用", false); err != nil {
		t.Fatal(err)
	}
	if e.current != "" {
		t.Fatal("paused group selected during first scan")
	}
	e.routes[c.Nodes[0].ID].Paused = false
	e.automatic = false
	if err := e.trySelect(context.Background(), c.Nodes[0].ID, "首次完整链路可用", false); err != nil {
		t.Fatal(err)
	}
	if e.current != "" {
		t.Fatal("manual mode permitted automatic selection")
	}
}
func TestBackgroundCancellation(t *testing.T) {
	w := fixtureWorkspace(t, false)
	started := make(chan struct{})
	id, err := w.startJob("fixture", "fixture", func(ctx context.Context, j *Job) error { close(started); <-ctx.Done(); return ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err = w.startJob("fixture", "duplicate", func(context.Context, *Job) error { return nil }); err == nil {
		t.Fatal("duplicate concurrent job accepted")
	}
	w.cancelJob(id)
	for i := 0; i < 100; i++ {
		jobs := w.jobViews()
		if jobs[0].State == "cancelled" {
			w.cancelBackground()
			return
		}
		time.Sleep(time.Millisecond * 5)
	}
	t.Fatal("cancel did not finish")
}
func TestRetiredNodeMigrationDoesNotResurrectMembership(t *testing.T) {
	c := defaults()
	c.Sources = []RelaySource{{ID: "s", Name: "s", Enabled: true, Priority: 100}}
	c.Nodes = []Node{{ID: "retired", Label: "old", Retired: true, Priority: 100, Proxy: map[string]any{"type": "socks5", "server": "relay.invalid", "port": 1080}}}
	w, err := newWorkspace(t.TempDir(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodeSources(w.snapshot().Nodes[0])) != 0 {
		t.Fatal("retired node assigned active source")
	}
}
