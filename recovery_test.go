package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRefreshRejectsConcurrentSourceChange(t *testing.T) {
	requested := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requested)
		<-release
		_, _ = w.Write([]byte("proxies:\n  - {name: fixture, type: socks5, server: relay.invalid, port: 1080}\n"))
	}))
	defer srv.Close()
	w := fixtureWorkspace(t, false)
	c := w.snapshot()
	s := RelaySource{ID: "s", Name: "s", URL: srv.URL, Enabled: true, Priority: 100}
	c.Sources = []RelaySource{s}
	if err := w.commit(c); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- w.refreshSource("s") }()
	<-requested
	s.URL = srv.URL + "/new"
	if err := w.saveSource(s); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-result; err == nil || !strings.Contains(err.Error(), "已修改") {
		t.Fatal("stale subscription response committed")
	}
	if len(w.snapshot().Nodes) != 0 {
		t.Fatal("stale nodes applied")
	}
}

func TestCoreCrashAutomaticallyRecoversWithoutOpeningRoute(t *testing.T) {
	w := fixtureWorkspace(t, true)
	c := w.snapshot()
	c.AutoRefresh = false
	c.IntervalSeconds = 86400
	c.Sources = []RelaySource{{ID: "s", Name: "s", Enabled: true, Priority: 100}}
	c.Nodes = []Node{{ID: "n", Label: "offline fixture", SourceID: "s", Priority: 100, Proxy: map[string]any{"type": "socks5", "server": "127.0.0.1", "port": 1}}}
	p := testProfile("recovery", 32380)
	p.Running = true
	p.FixedExit = map[string]any{"type": "socks5", "server": "127.0.0.1", "port": 2}
	c.Profiles = []ExitProfile{p}
	var err error
	w, err = newWorkspace(w.root, c)
	if err != nil {
		t.Fatal(err)
	}
	defer w.stopAll(false)
	if err = w.startProfile(p.ID); err != nil {
		t.Fatal(err)
	}
	e, _ := w.engine(p.ID)
	e.mu.Lock()
	old := e.cmd.Process
	pid := old.Pid
	e.mu.Unlock()
	if err = old.Kill(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.backgroundLoop(ctx)
	defer w.cancelBackground()
	until := time.Now().Add(9 * time.Second)
	for time.Now().Before(until) {
		e.mu.Lock()
		recovered := e.state == "running" && e.cmd != nil && e.cmd.Process.Pid != pid
		current := e.current
		e.mu.Unlock()
		if recovered {
			if current != "" {
				t.Fatal("unverified route opened after recovery")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("watchdog did not restart crashed test core")
}
