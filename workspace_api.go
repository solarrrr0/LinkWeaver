package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"time"

	"go.yaml.in/yaml/v3"
)

func (s *Server) targetEngine(r *http.Request) (*Engine, error) {
	if s.workspace != nil {
		return s.workspace.engine(r.URL.Query().Get("profile"))
	}
	if s.engine == nil {
		return nil, errors.New("工作区尚未初始化")
	}
	return s.engine, nil
}
func (s *Server) requireWorkspace(w http.ResponseWriter) bool {
	if s.workspace == nil {
		fail(w, errors.New("工作区尚未初始化"))
		return false
	}
	return true
}
func (s *Server) workspaceView(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	if !s.requireWorkspace(w) {
		return
	}
	c := s.workspace.snapshot()
	profiles := []map[string]any{}
	events := []map[string]any{}
	states := map[string]map[string]Route{}
	running, connected := 0, 0
	for _, p := range c.Profiles {
		e, err := s.workspace.engine(p.ID)
		if err != nil {
			continue
		}
		e.mu.Lock()
		by := map[string]Route{}
		rs := []Route{}
		for id, route := range e.routes {
			x := *route
			x.Samples = append([]Sample{}, route.Samples...)
			by[id] = x
			rs = append(rs, x)
		}
		states[p.ID] = by
		var cur any
		var latency float64
		if x, ok := by[e.current]; ok {
			cur = map[string]any{"id": x.ID, "name": x.Label, "protocol": x.Protocol}
			latency = x.EWMA
		}
		if e.state == "running" {
			running++
		}
		degraded := e.current != "" && by[e.current].ConsecutiveFailures >= 2
		if e.current != "" && !degraded {
			connected++
		}
		v := map[string]any{"id": p.ID, "name": p.Name, "protocol": p.FixedExit["type"], "proxyPort": p.ProxyPort, "state": e.state, "current": cur, "latencyMS": latency, "pinned": e.pinned, "identityVerified": e.current != "" && e.observedIP != "", "routeDegraded": degraded, "expectedIPConfigured": p.ExpectedIP != "", "scanning": e.scanning, "checked": e.checked, "total": e.total, "lastScan": e.lastScan, "lastError": e.lastError, "eligibleCount": len(e.routes), "routes": rs, "timing": e.timing, "allowedSources": p.AllowedSources, "restrictNodes": p.RestrictNodes, "allowedNodes": p.AllowedNodes, "selectionMode": p.SelectionMode, "manualRelayID": p.ManualRelayID}
		profiles = append(profiles, v)
		for _, ev := range e.events {
			events = append(events, map[string]any{"at": ev.At, "text": ev.Text, "profile": p.Name})
		}
		e.mu.Unlock()
	}
	nodes := []map[string]any{}
	sourceNames := map[string]string{}
	sourceCounts := map[string]int{}
	for _, src := range c.Sources {
		sourceNames[src.ID] = src.Name
	}
	for _, n := range c.Nodes {
		for _, id := range nodeSources(n) {
			sourceCounts[id]++
		}
		per := map[string]any{}
		for pid, rs := range states {
			if rt, ok := rs[n.ID]; ok {
				per[pid] = rt
			}
		}
		n = applyGroup(c, n)
		var health any
		s.workspace.mu.Lock()
		pool := s.workspace.pool
		s.workspace.mu.Unlock()
		if pool != nil {
			pool.mu.Lock()
			if rt := pool.routes[n.ID]; rt != nil {
				x := *rt
				x.Samples = append([]Sample{}, rt.Samples...)
				health = x
			}
			pool.mu.Unlock()
		}
		nodes = append(nodes, map[string]any{"id": n.ID, "name": n.Label, "protocol": n.Proxy["type"], "sourceID": n.SourceID, "sourceName": sourceNames[n.SourceID], "disabled": n.Disabled, "priority": nodePriority(n), "measurements": per, "sourceIDs": nodeSources(n), "retired": n.Retired, "groupPriority": n.GroupPriority, "groupName": n.GroupName, "paused": n.Paused, "health": health})
	}
	sources := []map[string]any{}
	for _, src := range c.Sources {
		hint := "本地节点"
		if u, err := url.Parse(src.URL); err == nil && u.Hostname() != "" {
			hint = u.Hostname()
		}
		sources = append(sources, map[string]any{"id": src.ID, "name": src.Name, "enabled": src.Enabled, "priority": src.Priority, "urlConfigured": src.URL != "", "endpointHint": hint, "userAgent": src.UserAgent, "updatedAt": src.UpdatedAt, "lastError": src.LastError, "nodeCount": sourceCounts[src.ID], "paused": src.Paused})
	}
	sort.Slice(events, func(i, j int) bool { return events[i]["at"].(time.Time).After(events[j]["at"].(time.Time)) })
	if len(events) > 30 {
		events = events[:30]
	}
	s.workspace.mu.Lock()
	tray := s.workspace.tray
	s.workspace.mu.Unlock()
	trayRegistered := tray != nil && tray.registered.Load()
	respond(w, map[string]any{"version": appVersion, "profiles": profiles, "nodes": nodes, "sources": sources, "events": events, "runningCount": running, "connectedCount": connected, "trayRegistered": trayRegistered, "settings": map[string]any{"speedPriority": c.SpeedPriority, "unifiedDelay": c.UnifiedDelay, "targets": c.Targets, "intervalSeconds": c.IntervalSeconds, "timeoutMS": c.TimeoutMS, "concurrency": c.Concurrency, "minGainMS": c.MinGainMS, "minGainPercent": c.MinGainPercent, "minDwellSeconds": c.MinDwellSeconds, "autoRefresh": c.AutoRefresh, "refreshMinutes": c.RefreshMinutes, "openOnLaunch": c.OpenOnLaunch, "resumeOnLaunch": c.ResumeOnLaunch, "runAtLogin": c.RunAtLogin, "autoRecover": c.AutoRecover}, "uiPort": c.UIPort, "dataDir": filepath.Join(s.workspace.root, "data"), "poolMode": c.PoolMode, "relayGroups": c.RelayGroups, "operations": s.workspace.jobViews()})
}

func (s *Server) profileSave(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	var p ExitProfile
	if err := decode(w, r, &p); err != nil {
		fail(w, err)
		return
	}
	if err := s.workspace.saveProfile(p); err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}
func readID(w http.ResponseWriter, r *http.Request) (string, error) {
	var v struct {
		ID string `json:"id"`
	}
	if err := decode(w, r, &v); err != nil {
		return "", err
	}
	if v.ID == "" {
		return "", errors.New("缺少项目 ID")
	}
	return v.ID, nil
}
func (s *Server) profileDelete(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	id, err := readID(w, r)
	if err == nil {
		err = s.workspace.deleteProfile(id)
	}
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}
func (s *Server) profileDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	if !s.requireWorkspace(w) {
		return
	}
	c := s.workspace.snapshot()
	for _, p := range c.Profiles {
		if p.ID == r.URL.Query().Get("id") {
			respond(w, map[string]any{"id": p.ID, "name": p.Name, "expectedIP": p.ExpectedIP, "proxyPort": p.ProxyPort, "fixedExit": safeProxy(p.FixedExit), "credentialConfigured": hasCredential(p.FixedExit), "allowedSources": p.AllowedSources, "restrictNodes": p.RestrictNodes, "allowedNodes": p.AllowedNodes, "selectionMode": p.SelectionMode, "manualRelayID": p.ManualRelayID})
			return
		}
	}
	fail(w, errors.New("出口不存在"))
}
func hasCredential(p map[string]any) bool {
	for k, v := range p {
		if secretKeys[k] && v != "" && v != nil {
			return true
		}
	}
	return false
}
func (s *Server) proxyParse(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) {
		return
	}
	var v struct {
		Value string `json:"value"`
	}
	if err := decode(w, r, &v); err != nil {
		fail(w, err)
		return
	}
	nodes, err := parseProxyText(v.Value)
	if err != nil {
		fail(w, err)
		return
	}
	if len(nodes) != 1 {
		fail(w, errors.New("请在出口中填写单条节点链接"))
		return
	}
	respond(w, map[string]any{"proxy": nodes[0]})
}
func (s *Server) relayImport(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	var v struct {
		Content  string `json:"content"`
		SourceID string `json:"sourceID"`
	}
	if err := decode(w, r, &v); err != nil {
		fail(w, err)
		return
	}
	n, err := s.workspace.importRelays(v.Content, v.SourceID)
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]any{"ok": true, "count": n})
}
func (s *Server) relaySave(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	var n Node
	if err := decode(w, r, &n); err != nil {
		fail(w, err)
		return
	}
	if err := s.workspace.saveNode(n); err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}
func (s *Server) relayDelete(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	id, err := readID(w, r)
	if err == nil {
		err = s.workspace.deleteNode(id)
	}
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}
func (s *Server) relayDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	if !s.requireWorkspace(w) {
		return
	}
	for _, n := range s.workspace.snapshot().Nodes {
		if n.ID == r.URL.Query().Get("id") {
			respond(w, map[string]any{"id": n.ID, "label": n.Label, "disabled": n.Disabled, "priority": nodePriority(n), "proxy": safeProxy(n.Proxy), "credentialConfigured": hasCredential(n.Proxy)})
			return
		}
	}
	fail(w, errors.New("中转不存在"))
}
func (s *Server) relayTest(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) {
		return
	}
	id, err := readID(w, r)
	if err != nil {
		fail(w, err)
		return
	}
	e, err := s.targetEngine(r)
	if err != nil {
		fail(w, err)
		return
	}
	ctx, err := e.runCtx()
	if err != nil {
		fail(w, err)
		return
	}
	e.mu.Lock()
	exists := e.routes[id] != nil
	e.mu.Unlock()
	if !exists {
		fail(w, errors.New("此中转未启用或不属于该出口候选范围"))
		return
	}
	sample := e.probe(ctx, id, e.cfg().Targets[0])
	e.mu.Lock()
	e.routes[id].Record(sample)
	e.mu.Unlock()
	respond(w, sample)
}
func (s *Server) sourceSave(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	var src RelaySource
	if err := decode(w, r, &src); err != nil {
		fail(w, err)
		return
	}
	if err := s.workspace.saveSource(src); err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}
func (s *Server) sourceDelete(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	id, err := readID(w, r)
	if err == nil {
		err = s.workspace.deleteSource(id)
	}
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}
func (s *Server) sourceRefresh(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	id, err := readID(w, r)
	if err == nil {
		err = s.workspace.refreshSource(id)
	}
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}

func (s *Server) workspaceSettings(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) {
		return
	}
	var v struct {
		Targets         []string `json:"targets"`
		SpeedPriority   bool     `json:"speedPriority"`
		UnifiedDelay    bool     `json:"unifiedDelay"`
		IntervalSeconds int      `json:"intervalSeconds"`
		TimeoutMS       int      `json:"timeoutMS"`
		Concurrency     int      `json:"concurrency"`
		MinGainMS       int      `json:"minGainMS"`
		MinGainPercent  int      `json:"minGainPercent"`
		MinDwellSeconds int      `json:"minDwellSeconds"`
		AutoRefresh     bool     `json:"autoRefresh"`
		RefreshMinutes  int      `json:"refreshMinutes"`
		OpenOnLaunch    bool     `json:"openOnLaunch"`
		ResumeOnLaunch  bool     `json:"resumeOnLaunch"`
		RunAtLogin      bool     `json:"runAtLogin"`
		AutoRecover     bool     `json:"autoRecover"`
	}
	if err := decode(w, r, &v); err != nil {
		fail(w, err)
		return
	}
	m := s.workspace
	m.op.Lock()
	defer m.op.Unlock()
	c := m.snapshot()
	oldLogin := c.RunAtLogin
	c.Targets = v.Targets
	c.SpeedPriority = v.SpeedPriority
	c.UnifiedDelay = v.UnifiedDelay
	c.IntervalSeconds = v.IntervalSeconds
	c.TimeoutMS = v.TimeoutMS
	c.Concurrency = v.Concurrency
	c.MinGainMS = v.MinGainMS
	c.MinGainPercent = v.MinGainPercent
	c.MinDwellSeconds = v.MinDwellSeconds
	c.AutoRefresh = v.AutoRefresh
	c.RefreshMinutes = v.RefreshMinutes
	c.OpenOnLaunch = v.OpenOnLaunch
	c.ResumeOnLaunch = v.ResumeOnLaunch
	c.RunAtLogin = v.RunAtLogin
	c.AutoRecover = v.AutoRecover
	if err := validateWorkspace(c); err != nil {
		fail(w, err)
		return
	}
	if err := setLoginStartup(m.root, c.RunAtLogin); err != nil {
		fail(w, err)
		return
	}
	if err := m.commit(c); err != nil {
		if oldLogin != c.RunAtLogin {
			_ = setLoginStartup(m.root, oldLogin)
		}
		fail(w, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}
func (s *Server) backup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	if !s.requireWorkspace(w) {
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="LinkWeaver-backup-%s.json"`, time.Now().Format("20060102")))
	respond(w, s.workspace.snapshot())
}
func (s *Server) workspaceImport(w http.ResponseWriter, r *http.Request) {
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
		fail(w, errors.New("备份文件格式无效"))
		return
	}
	if len(c.Profiles) == 0 && c.FixedExit != nil {
		c.Profiles = []ExitProfile{{ID: "default", Name: c.Name, ExpectedIP: c.ExpectedIP, FixedExit: c.FixedExit, ProxyPort: c.ProxyPort, VerifyPort: c.VerifyPort, ControllerPort: c.ControllerPort}}
		c.FixedExit = nil
	}
	if c.RefreshMinutes == 0 {
		c.RefreshMinutes = 360
	}
	if len(c.Sources) == 0 {
		c.Sources = []RelaySource{{ID: "manual", Name: "导入的中转", Enabled: true, Priority: 100}}
	}
	for i := range c.Nodes {
		if c.Nodes[i].SourceID == "" {
			c.Nodes[i].SourceID = c.Sources[0].ID
		}
	}
	c.Version = 2
	m := s.workspace
	m.op.Lock()
	defer m.op.Unlock()
	if c.UIPort != m.snapshot().UIPort {
		fail(w, errors.New("备份中的管理端口不同，请退出后手动恢复"))
		return
	}
	oldLogin := m.snapshot().RunAtLogin
	if err := validateWorkspace(c); err != nil {
		fail(w, err)
		return
	}
	if err := setLoginStartup(m.root, c.RunAtLogin); err != nil {
		fail(w, err)
		return
	}
	if err := m.commit(c); err != nil {
		if oldLogin != c.RunAtLogin {
			_ = setLoginStartup(m.root, oldLogin)
		}
		fail(w, err)
		return
	}
	respond(w, map[string]bool{"ok": true})
}
