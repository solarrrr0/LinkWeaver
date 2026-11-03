package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"go.yaml.in/yaml/v3"
)

func TestNativeClashIntegrationAndRollback(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint(reject), func(t *testing.T) {
			w := fixtureWorkspace(t, true)
			root := t.TempDir()
			pipe := `\\.\pipe\verge-mihomo-linkweaver-fixture-` + randomToken()[:16]
			listener, err := winio.ListenPipe(pipe, nil)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			selection := "old"
			reloads := 0
			var applied map[string]any
			srv := &http.Server{Handler: http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("Authorization") != "Bearer fixture" {
					out.WriteHeader(401)
					return
				}
				switch {
				case r.URL.Path == "/version":
					respond(out, map[string]string{"version": "fixture"})
				case r.URL.Path == "/configs" && r.Method == "GET":
					respond(out, map[string]string{"mode": "rule"})
				case r.URL.Path == "/proxies" && r.Method == "GET":
					respond(out, map[string]any{"proxies": map[string]any{"manual": map[string]any{"type": "Selector", "now": selection}}})
				case r.URL.Path == "/configs" && r.Method == "PUT":
					reloads++
					if reject && reloads == 1 {
						out.WriteHeader(500)
						return
					}
					var v struct {
						Payload string `json:"payload"`
					}
					_ = json.NewDecoder(r.Body).Decode(&v)
					_ = yaml.Unmarshal([]byte(v.Payload), &applied)
					selection = "DIRECT"
					out.WriteHeader(204)
				case r.URL.Path == "/proxies/manual" && r.Method == "PUT":
					var v struct {
						Name string `json:"name"`
					}
					_ = json.NewDecoder(r.Body).Decode(&v)
					selection = v.Name
					out.WriteHeader(204)
				default:
					out.WriteHeader(404)
				}
			})}
			go srv.Serve(listener)
			defer srv.Close()
			script := "function main(config) { config.fixture = true; return config; }\n"
			_ = os.MkdirAll(filepath.Join(root, "profiles"), 0700)
			_ = os.WriteFile(filepath.Join(root, "profiles", "s.js"), []byte(script), 0600)
			profiles := map[string]any{"current": "remote", "items": []any{map[string]any{"uid": "remote", "type": "remote", "option": map[string]any{"script": "s"}}, map[string]any{"uid": "s", "type": "script", "file": "s.js"}}}
			b, _ := yaml.Marshal(profiles)
			_ = os.WriteFile(filepath.Join(root, "profiles.yaml"), b, 0600)
			doc := map[string]any{"mixed-port": 0, "mode": "rule", "secret": "fixture", "external-controller-pipe": pipe, "dns": map[string]any{"enable": false}, "proxies": []any{map[string]any{"name": "old", "type": "socks5", "server": "127.0.0.1", "port": 33333}}, "proxy-groups": []any{map[string]any{"name": "manual", "type": "select", "proxies": []any{"DIRECT", "old"}}, map[string]any{"name": "auto", "type": "url-test", "proxies": []any{"old"}, "url": "https://example.invalid", "interval": 300}}, "rules": []any{"MATCH,manual"}}
			b, _ = yaml.Marshal(doc)
			_ = os.WriteFile(filepath.Join(root, "clash-verge.yaml"), b, 0600)
			adapter, err := discoverVerge(root)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			count, err := w.integrateClash(ctx, []ClashEntrance{{Name: "链跃 · fixture", Port: 27892}}, adapter)
			after, _ := os.ReadFile(adapter.script)
			mu.Lock()
			defer mu.Unlock()
			if reject {
				if err == nil || !strings.Contains(err.Error(), "已回滚") || string(after) != script || reloads != 2 {
					t.Fatalf("reload failure did not roll back: %v reloads=%d", err, reloads)
				}
			} else {
				if err != nil || count != 1 || !strings.Contains(string(after), clashBlockBegin) {
					t.Fatalf("integration failed: %v", err)
				}
				groups := applied["proxy-groups"].([]any)
				if len(groups[0].(map[string]any)["proxies"].([]any)) != 3 || len(groups[1].(map[string]any)["proxies"].([]any)) != 1 {
					t.Fatal("non-selector group changed or missing entry")
				}
				if applied["rules"].([]any)[0] != "MATCH,manual" || applied["dns"].(map[string]any)["enable"] != false {
					t.Fatal("rules or DNS lost")
				}
			}
			if selection != "old" {
				t.Fatal("current selection changed")
			}
		})
	}
}

func TestClashScriptWrapperExecution(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		node = `D:\DevTools\nodejs\node.exe`
		if _, err = os.Stat(node); err != nil {
			t.Skip("Node unavailable")
		}
	}
	original := "function main(config, name) { config.called = name; return config; }"
	entries := []ClashEntrance{{Name: "链跃 · fixture", Port: 27892}}
	once, err := injectClashScript(original, entries)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := injectClashScript(once, entries)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(twice, clashBlockBegin) != 1 {
		t.Fatal("duplicate managed wrapper")
	}
	script := twice + `\nconst assert=require('node:assert/strict');const cfg={proxies:[{name:'old'}],'proxy-groups':[{type:'select',proxies:['old']},{type:'url-test',proxies:['old']}],rules:['MATCH,DIRECT']};let result=main(cfg,'original');result=main(result,'original');assert.equal(result.called,'original');assert.equal(result.proxies.length,2);assert.equal(result['proxy-groups'][0].proxies.length,2);assert.equal(result['proxy-groups'][1].proxies.length,1);assert.deepEqual(result.rules,['MATCH,DIRECT']);`
	script = strings.ReplaceAll(script, `\nconst assert`, "\nconst assert")
	path := filepath.Join(t.TempDir(), "script.cjs")
	_ = os.WriteFile(path, []byte(script), 0600)
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("wrapper failed: %v %s", err, out)
	}
	if _, err := injectClashScript(original+clashBlockBegin, entries); err == nil {
		t.Fatal("accepted broken existing block")
	}
}
