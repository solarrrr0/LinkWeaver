package main

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"
)

//go:embed web/*
var assets embed.FS

type Server struct {
	engine       *Engine
	workspace    *Workspace
	token        string
	shutdown     chan struct{}
	clashKeyOnce sync.Once
	clashKey     string
	clashKeyErr  error
	openClashURI func(string) error
	updateMu     sync.Mutex
	online       onlineUpdateState
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.page)
	mux.HandleFunc("/dashboard.js", s.asset)
	mux.HandleFunc("/dashboard.css", s.asset)
	mux.HandleFunc("/features.js", s.asset)
	mux.HandleFunc("/api/status", s.status)
	mux.HandleFunc("/api/start", s.start)
	mux.HandleFunc("/api/stop", s.stop)
	mux.HandleFunc("/api/scan", s.scan)
	mux.HandleFunc("/api/select", s.selectRoute)
	mux.HandleFunc("/api/measure", s.measure)
	mux.HandleFunc("/api/import", s.importConfig)
	mux.HandleFunc("/api/settings", s.settings)
	mux.HandleFunc("/api/shutdown", s.quit)
	mux.HandleFunc("/api/update/status", s.updateStatus)
	mux.HandleFunc("/api/update/check", s.updateCheck)
	mux.HandleFunc("/api/update/online", s.updateOnline)
	mux.HandleFunc("/api/update/prepare", s.updatePrepare)
	mux.HandleFunc("/api/update/apply", s.updateApply)
	mux.HandleFunc("/api/update/rollback", s.updateRollback)
	mux.HandleFunc("/api/workspace", s.workspaceView)
	mux.HandleFunc("/api/profiles/save", s.profileSave)
	mux.HandleFunc("/api/profiles/delete", s.profileDelete)
	mux.HandleFunc("/api/profiles/detail", s.profileDetail)
	mux.HandleFunc("/api/proxy/parse", s.proxyParse)
	mux.HandleFunc("/api/relays/import", s.relayImport)
	mux.HandleFunc("/api/relays/save", s.relaySave)
	mux.HandleFunc("/api/relays/delete", s.relayDelete)
	mux.HandleFunc("/api/relays/detail", s.relayDetail)
	mux.HandleFunc("/api/relays/test", s.relayTest)
	mux.HandleFunc("/api/sources/save", s.sourceSave)
	mux.HandleFunc("/api/sources/delete", s.sourceDelete)
	mux.HandleFunc("/api/sources/refresh", s.sourceRefresh)
	mux.HandleFunc("/api/backup", s.backup)
	mux.HandleFunc("/api/clash/export", s.clashExportInfo)
	mux.HandleFunc("/api/groups/save", s.groupsSave)
	mux.HandleFunc("/api/groups/delete", s.groupsDelete)
	mux.HandleFunc("/api/jobs/start", s.jobsStart)
	mux.HandleFunc("/api/jobs/cancel", s.jobsCancel)
	mux.HandleFunc("/api/clash/install", s.clashInstall)
	mux.HandleFunc("/api/clash/integration", s.clashIntegrationStatus)
	mux.HandleFunc("/api/clash/integrate", s.clashIntegrate)
	mux.HandleFunc("/clash/subscription.yaml", s.clashSubscription)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h, _, err := net.SplitHostPort(r.Host)
		if err != nil || h != "127.0.0.1" {
			http.Error(w, "Invalid host", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if r.Header.Get("X-LinkWeaver-Token") != s.token {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
				http.Error(w, "Invalid origin", http.StatusForbidden)
				return
			}
			if r.Method != http.MethodGet && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
func post(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return false
	}
	return true
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 3*1024*1024)
	defer r.Body.Close()
	d := json.NewDecoder(r.Body)
	if err := d.Decode(v); err != nil {
		return errors.New("提交内容格式无效或过大")
	}
	return nil
}
func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	b, _ := assets.ReadFile("web/dashboard.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, strings.ReplaceAll(string(b), "{{TOKEN}}", s.token))
}
func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	b, err := assets.ReadFile("web" + r.URL.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, ".js") {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	}
	_, _ = w.Write(b)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	e, err := s.targetEngine(r)
	if err != nil {
		fail(w, err)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	routes := []Route{}
	for _, x := range e.routes {
		c := *x
		c.Samples = append([]Sample{}, x.Samples...)
		routes = append(routes, c)
	}
	sort.Slice(routes, func(i, j int) bool {
		a, b := routes[i], routes[j]
		if a.ID == b.ID {
			return false
		}
		if a.ID == e.current {
			return true
		}
		if b.ID == e.current {
			return false
		}
		if a.Status != b.Status {
			if a.Status == "available" {
				return true
			}
			if b.Status == "available" {
				return false
			}
		}
		if a.Score > 0 && b.Score > 0 && a.Score != b.Score {
			return a.Score < b.Score
		}
		return a.ID < b.ID
	})
	c := e.config
	view := map[string]any{"version": appVersion, "state": e.state, "name": c.Name, "current": e.current, "pinned": e.pinned, "proxyPort": c.ProxyPort, "scanning": e.scanning, "checked": e.checked, "total": e.total, "lastScan": e.lastScan, "routes": routes, "events": e.events, "timing": e.timing, "expectedIPConfigured": c.ExpectedIP != "", "identityVerified": e.current != "" && e.observedIP != "", "lastError": e.lastError, "lastSwitch": e.lastSwitch, "configPath": filepath.Join(e.root, "data", "config.json"), "settings": map[string]any{"targets": c.Targets, "intervalSeconds": c.IntervalSeconds, "timeoutMS": c.TimeoutMS, "concurrency": c.Concurrency, "minGainMS": c.MinGainMS, "minGainPercent": c.MinGainPercent, "minDwellSeconds": c.MinDwellSeconds}}
	respond(w, view)
}
func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) {
		return
	}
	var err error
	if s.workspace != nil {
		err = s.workspace.startProfile(r.URL.Query().Get("profile"))
	} else {
		err = s.engine.Start()
	}
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}
func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) {
		return
	}
	if s.workspace != nil {
		if err := s.workspace.stopProfile(r.URL.Query().Get("profile")); err != nil {
			fail(w, err)
			return
		}
	} else {
		s.engine.Stop()
	}
	respond(w, map[string]bool{"ok": true})
}
func (s *Server) scan(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) {
		return
	}
	e, err := s.targetEngine(r)
	if err != nil {
		fail(w, err)
		return
	}
	if err := e.RequestScan(); err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}
func (s *Server) selectRoute(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) {
		return
	}
	var v struct {
		ID string `json:"id"`
	}
	if err := decode(w, r, &v); err != nil {
		fail(w, err)
		return
	}
	e, err := s.targetEngine(r)
	if err != nil {
		fail(w, err)
		return
	}
	selectErr := error(nil)
	if s.workspace != nil {
		selectErr = s.workspace.setSelection(r.URL.Query().Get("profile"), v.ID)
	} else {
		selectErr = e.ManualSelect(v.ID)
	}
	if err := selectErr; err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}
func (s *Server) measure(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) {
		return
	}
	e, err := s.targetEngine(r)
	if err != nil {
		fail(w, err)
		return
	}
	t, err := e.Measure()
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, t)
}

func (s *Server) importConfig(w http.ResponseWriter, r *http.Request) {
	if s.workspace != nil {
		s.workspaceImport(w, r)
		return
	}
	if !post(w, r) {
		return
	}
	var v struct {
		Content string `json:"content"`
	}
	if err := decode(w, r, &v); err != nil {
		fail(w, err)
		return
	}
	c := defaults()
	if err := yaml.Unmarshal([]byte(v.Content), &c); err != nil {
		fail(w, errors.New("导入格式无效；请选择 LinkWeaver JSON/YAML 配置"))
		return
	}
	if len(c.Nodes) == 0 {
		fail(w, errors.New("没有发现 nodes；普通订阅 YAML 需要转换为 LinkWeaver 配置"))
		return
	}
	if err := c.Validate(); err != nil {
		fail(w, err)
		return
	}
	e := s.engine
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state != "stopped" && e.state != "error" {
		fail(w, errors.New("请先停止代理再导入配置"))
		return
	}
	if c.UIPort != e.config.UIPort {
		fail(w, errors.New("界面端口变更请在退出后编辑本地配置文件"))
		return
	}
	path := filepath.Join(e.root, "data", "config.json")
	if b, err := os.ReadFile(path); err == nil {
		if err = atomicWrite(filepath.Join(e.root, "data", "config.previous.json"), b); err != nil {
			fail(w, errors.New("无法备份原有配置"))
			return
		}
	}
	if err := saveConfig(path, c); err != nil {
		fail(w, errors.New("保存配置失败"))
		return
	}
	e.config = c
	e.resetRoutes()
	e.eventLocked("配置已导入，原有配置已保留一份本地备份")
	respond(w, map[string]any{"ok": true, "count": len(c.Nodes)})
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	if s.workspace != nil {
		s.workspaceSettings(w, r)
		return
	}
	if !post(w, r) {
		return
	}
	var v struct {
		Targets         []string `json:"targets"`
		IntervalSeconds int      `json:"intervalSeconds"`
		TimeoutMS       int      `json:"timeoutMS"`
		Concurrency     int      `json:"concurrency"`
		MinGainMS       int      `json:"minGainMS"`
		MinGainPercent  int      `json:"minGainPercent"`
		MinDwellSeconds int      `json:"minDwellSeconds"`
	}
	if err := decode(w, r, &v); err != nil {
		fail(w, err)
		return
	}
	e := s.engine
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.scanning {
		fail(w, errors.New("请在本轮检测结束后修改参数"))
		return
	}
	c := e.config
	c.Targets = v.Targets
	c.IntervalSeconds = v.IntervalSeconds
	c.TimeoutMS = v.TimeoutMS
	c.Concurrency = v.Concurrency
	c.MinGainMS = v.MinGainMS
	c.MinGainPercent = v.MinGainPercent
	c.MinDwellSeconds = v.MinDwellSeconds
	if err := c.Validate(); err != nil {
		fail(w, err)
		return
	}
	if err := saveConfig(filepath.Join(e.root, "data", "config.json"), c); err != nil {
		fail(w, errors.New("保存配置失败"))
		return
	}
	changed := strings.Join(c.Targets, "\n") != strings.Join(e.config.Targets, "\n")
	e.config = c
	if changed {
		for _, route := range e.routes {
			route.Samples = []Sample{}
			route.EWMA = 0
			route.Jitter = 0
			route.Successes = 0
			route.Failures = 0
			route.ConsecutiveFailures = 0
			route.ConsecutiveSuccesses = 0
			route.Status = "unknown"
			route.Score = 0
		}
		e.eventLocked("测试目标已修改，历史评分已清空；下一轮重新测量")
	}
	respond(w, map[string]bool{"ok": true})
}
func (s *Server) quit(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) {
		return
	}
	respond(w, map[string]bool{"ok": true})
	select {
	case s.shutdown <- struct{}{}:
	default:
	}
}
func listenAddr(port int) string { return "127.0.0.1:" + strconv.Itoa(port) }
