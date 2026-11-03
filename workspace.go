package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
)

type ExitProfile struct {
	ID             string         `json:"id" yaml:"id"`
	Name           string         `json:"name" yaml:"name"`
	ExpectedIP     string         `json:"expectedIP" yaml:"expectedIP"`
	FixedExit      map[string]any `json:"fixedExit" yaml:"fixedExit"`
	ProxyPort      int            `json:"proxyPort" yaml:"proxyPort"`
	VerifyPort     int            `json:"verifyPort" yaml:"verifyPort"`
	ControllerPort int            `json:"controllerPort" yaml:"controllerPort"`
	AllowedSources []string       `json:"allowedSources" yaml:"allowedSources"`
	Running        bool           `json:"running" yaml:"running"`
	RestrictNodes  bool           `json:"restrictNodes" yaml:"restrictNodes"`
	AllowedNodes   []string       `json:"allowedNodes" yaml:"allowedNodes"`
	SelectionMode  string         `json:"selectionMode" yaml:"selectionMode"`
	ManualRelayID  string         `json:"manualRelayID" yaml:"manualRelayID"`
}
type RelaySource struct {
	ID        string    `json:"id" yaml:"id"`
	Name      string    `json:"name" yaml:"name"`
	URL       string    `json:"url" yaml:"url"`
	Enabled   bool      `json:"enabled" yaml:"enabled"`
	Priority  int       `json:"priority" yaml:"priority"`
	UserAgent string    `json:"userAgent" yaml:"userAgent"`
	UpdatedAt time.Time `json:"updatedAt" yaml:"updatedAt"`
	LastError string    `json:"lastError" yaml:"lastError"`
	Paused    bool      `json:"paused" yaml:"paused"`
}
type Workspace struct {
	mu      sync.Mutex
	op      sync.Mutex
	root    string
	config  Config
	engines map[string]*Engine
	tray    *Tray
	jobs    map[string]*Job
	closing bool
	pool    *Engine
	poolMu  sync.Mutex
}

func nodePriority(n Node) int {
	if n.Priority == 0 {
		return 100
	}
	return n.Priority
}
func copyConfig(c Config) Config {
	b, _ := json.Marshal(c)
	var out Config
	_ = json.Unmarshal(b, &out)
	return out
}

func newWorkspace(root string, c Config) (*Workspace, error) {
	if len(c.Profiles) == 0 && c.FixedExit != nil {
		c.Profiles = []ExitProfile{{ID: "default", Name: c.Name, ExpectedIP: c.ExpectedIP, FixedExit: cloneMap(c.FixedExit), ProxyPort: c.ProxyPort, VerifyPort: c.VerifyPort, ControllerPort: c.ControllerPort, Running: true}}
		c.FixedExit = nil
		c.ExpectedIP = ""
		c.Version = 2
	}
	if c.RefreshMinutes == 0 {
		c.RefreshMinutes = 360
	}
	if c.Profiles == nil {
		c.Profiles = []ExitProfile{}
	}
	if c.Sources == nil {
		c.Sources = []RelaySource{}
	}
	for i := range c.Nodes {
		if c.Nodes[i].SourceID == "" && !c.Nodes[i].Retired && len(c.Nodes[i].SourceIDs) == 0 {
			c.Nodes[i].SourceID = "migrated"
		}
		if c.Nodes[i].Priority == 0 {
			c.Nodes[i].Priority = 100
		}
	}
	if len(c.Nodes) > 0 && len(c.Sources) == 0 {
		c.Sources = []RelaySource{{ID: "migrated", Name: "原 ChainPilot 中转", Enabled: true, Priority: 100}}
	}
	c.Version = 3
	if c.PoolMode == "" {
		c.PoolMode = "source"
	}
	w := &Workspace{root: root, config: c, engines: map[string]*Engine{}}
	if err := validateWorkspace(c); err != nil {
		return nil, err
	}
	for _, p := range c.Profiles {
		e := newEngine(root, profileConfig(c, p))
		e.scope = p.ID
		w.engines[p.ID] = e
	}
	if err := saveConfig(filepath.Join(root, "data", "config.json"), c); err != nil {
		return nil, err
	}
	return w, nil
}

func profileConfig(c Config, p ExitProfile) Config {
	c = copyConfig(c)
	nodes := []Node{}
	for _, n := range c.Nodes {
		if n.Disabled || n.Retired {
			continue
		}
		enabled, _, _, _ := sourceNode(c, n, p.AllowedSources)
		if !enabled {
			continue
		}
		if p.RestrictNodes && !slices.Contains(p.AllowedNodes, n.ID) {
			continue
		}
		n = applyGroup(c, n)
		if c.PoolMode != "custom" {
			_, n.GroupPriority, n.GroupName, n.Paused = sourceNode(c, n, p.AllowedSources)
		}
		nodes = append(nodes, n)
	}
	c.Nodes = nodes
	c.Profiles = nil
	c.Sources = nil
	c.SelectionMode = p.SelectionMode
	c.ManualRelayID = p.ManualRelayID
	c.Name = p.Name
	c.ExpectedIP = p.ExpectedIP
	c.FixedExit = cloneMap(p.FixedExit)
	c.ProxyPort = p.ProxyPort
	c.VerifyPort = p.VerifyPort
	c.ControllerPort = p.ControllerPort
	return c
}

func validateWorkspace(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := validateGroups(c); err != nil {
		return err
	}
	if len(c.Profiles) > 8 {
		return errors.New("最多支持 8 个出口")
	}
	if c.RefreshMinutes < 5 || c.RefreshMinutes > 43200 {
		return errors.New("订阅刷新间隔应为 5–43200 分钟")
	}
	ports := map[int]bool{c.UIPort: true}
	ids := map[string]bool{}
	for _, p := range c.Profiles {
		if p.ID == "" || strings.ContainsAny(p.ID, "/\\ .") || ids[p.ID] {
			return errors.New("出口 ID 无效或重复")
		}
		ids[p.ID] = true
		if strings.TrimSpace(p.Name) == "" {
			return errors.New("请填写出口名称")
		}
		if p.SelectionMode != "" && p.SelectionMode != "automatic" && p.SelectionMode != "manual" {
			return errors.New("出口选择模式无效")
		}
		if p.SelectionMode == "manual" && p.ManualRelayID == "" {
			return errors.New("手动模式需要指定锁定中转")
		}
		cfg := profileConfig(c, p)
		if err := cfg.Validate(); err != nil {
			return err
		}
		if err := validateProxy(p.FixedExit); err != nil {
			return fmt.Errorf("出口配置不完整：%w", err)
		}
		for _, port := range []int{p.ProxyPort, p.VerifyPort, p.ControllerPort} {
			if ports[port] {
				return fmt.Errorf("端口 %d 与其他出口冲突", port)
			}
			ports[port] = true
		}
	}
	sourceIDs := map[string]bool{}
	for _, s := range c.Sources {
		if s.ID == "" || sourceIDs[s.ID] {
			return errors.New("来源 ID 无效或重复")
		}
		sourceIDs[s.ID] = true
		if strings.TrimSpace(s.Name) == "" || s.Priority < 1 || s.Priority > 10000 {
			return errors.New("来源名称不能为空，优先级范围 1–10000")
		}
		if s.URL != "" {
			u, err := url.Parse(s.URL)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
				return errors.New("订阅地址必须是 HTTP 或 HTTPS 地址")
			}
		}
	}
	return nil
}

func normalizeNodes(c *Config) {
	// A disabled subscription keeps its cached nodes, but engines exclude them.
	for i := range c.Nodes {
		if c.Nodes[i].Priority == 0 {
			c.Nodes[i].Priority = 100
		}
	}
}
func engineConfig(c Config, p ExitProfile) Config {
	return profileConfig(c, p)
}

func (w *Workspace) snapshot() Config { w.mu.Lock(); defer w.mu.Unlock(); return copyConfig(w.config) }
func (w *Workspace) engine(id string) (*Engine, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if id == "" && len(w.config.Profiles) > 0 {
		id = w.config.Profiles[0].ID
	}
	e := w.engines[id]
	if e == nil {
		return nil, errors.New("出口不存在，请先添加出口")
	}
	return e, nil
}

func (w *Workspace) commit(c Config) error {
	normalizeNodes(&c)
	if err := validateWorkspace(c); err != nil {
		return err
	}
	w.mu.Lock()
	old := copyConfig(w.config)
	oldEngines := map[string]*Engine{}
	for id, e := range w.engines {
		oldEngines[id] = e
	}
	w.mu.Unlock()
	// Validate protocol configurations before changing running cores or persistent data.
	for _, p := range c.Profiles {
		cfg := engineConfig(c, p)
		if len(cfg.Nodes) == 0 {
			continue
		}
		if e := oldEngines[p.ID]; e != nil && sameCore(e.cfg(), cfg) {
			continue
		}
		if err := w.checkCore(cfg); err != nil {
			return err
		}
	}
	path := filepath.Join(w.root, "data", "config.json")
	if err := saveConfig(filepath.Join(w.root, "data", "config.previous.json"), old); err != nil {
		return errors.New("无法保存配置备份")
	}
	if err := saveConfig(path, c); err != nil {
		return errors.New("无法保存配置")
	}
	next := map[string]*Engine{}
	resume := []*Engine{}
	for _, p := range c.Profiles {
		cfg := engineConfig(c, p)
		e := oldEngines[p.ID]
		if e != nil && sameCore(e.cfg(), cfg) && reflect.DeepEqual(e.cfg().Targets, cfg.Targets) {
			e.mu.Lock()
			e.config = cfg
			for _, n := range cfg.Nodes {
				if route := e.routes[n.ID]; route != nil {
					route.Label = n.Label
					route.Priority = nodePriority(n)
					route.GroupPriority = n.GroupPriority
					route.GroupName = n.GroupName
					route.Paused = n.Paused
				}
			}
			e.mu.Unlock()
			next[p.ID] = e
			delete(oldEngines, p.ID)
			continue
		}
		wasRunning := p.Running
		if e != nil {
			e.mu.Lock()
			wasRunning = e.state == "running" || e.state == "starting"
			e.mu.Unlock()
			e.Stop()
			delete(oldEngines, p.ID)
		}
		e = newEngine(w.root, cfg)
		e.scope = p.ID
		next[p.ID] = e
		if wasRunning && len(cfg.Nodes) > 0 {
			resume = append(resume, e)
		}
	}
	for _, e := range oldEngines {
		e.Stop()
	}
	w.mu.Lock()
	w.config = c
	w.engines = next
	w.mu.Unlock()
	for _, e := range resume {
		if err := e.Start(); err != nil {
			e.mu.Lock()
			e.state = "error"
			e.lastError = err.Error()
			e.mu.Unlock()
		}
	}
	return nil
}

func sameCore(a, b Config) bool {
	return a.ExpectedIP == b.ExpectedIP && reflect.DeepEqual(buildCoreConfig(a, ""), buildCoreConfig(b, ""))
}

func (w *Workspace) checkCore(c Config) error {
	dir := filepath.Join(w.root, "runtime", "validation")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("无法创建校验目录")
	}
	f, err := os.CreateTemp(dir, "candidate-*.json")
	if err != nil {
		return errors.New("无法创建配置校验文件")
	}
	path := f.Name()
	b, _ := json.Marshal(buildCoreConfig(c, "validation-only"))
	_, err = f.Write(b)
	f.Close()
	defer os.Remove(path)
	if err != nil {
		return errors.New("无法写入校验配置")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(w.root, "bin", "mihomo.exe"), "-t", "-d", dir, "-f", path)
	hideWindow(cmd)
	if err = cmd.Run(); err != nil {
		return errors.New("代理配置未通过核心校验，请检查协议、认证和高级参数")
	}
	return nil
}

func (w *Workspace) setDesired(id string, running bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := copyConfig(w.config)
	for i := range c.Profiles {
		if c.Profiles[i].ID == id {
			c.Profiles[i].Running = running
		}
	}
	if err := saveConfig(filepath.Join(w.root, "data", "config.json"), c); err != nil {
		return errors.New("保存运行状态失败")
	}
	w.config = c
	return nil
}
func (w *Workspace) startProfile(id string) error {
	w.op.Lock()
	defer w.op.Unlock()
	e, err := w.engine(id)
	if err != nil {
		return err
	}
	if id == "" {
		id = e.scope
	}
	if err = e.Start(); err != nil {
		e.mu.Lock()
		e.lastError = err.Error()
		if e.state != "running" {
			e.state = "error"
		}
		e.mu.Unlock()
		return err
	}
	return w.setDesired(id, true)
}
func (w *Workspace) stopProfile(id string) error {
	w.op.Lock()
	defer w.op.Unlock()
	e, err := w.engine(id)
	if err != nil {
		return err
	}
	if id == "" {
		id = e.scope
	}
	e.Stop()
	return w.setDesired(id, false)
}
func (w *Workspace) restore(force bool) {
	c := w.snapshot()
	for _, p := range c.Profiles {
		if (c.ResumeOnLaunch || force) && p.Running {
			_ = w.startProfile(p.ID)
		}
	}
}
func (w *Workspace) stopAll(persist bool) {
	w.op.Lock()
	defer w.op.Unlock()
	c := w.snapshot()
	for _, p := range c.Profiles {
		if e, err := w.engine(p.ID); err == nil {
			e.Stop()
		}
		if persist {
			_ = w.setDesired(p.ID, false)
		}
	}
}
func (w *Workspace) startAll() {
	c := w.snapshot()
	for _, p := range c.Profiles {
		e, err := w.engine(p.ID)
		if err != nil {
			continue
		}
		e.mu.Lock()
		running := e.state == "running"
		e.mu.Unlock()
		if !running {
			_ = w.startProfile(p.ID)
		}
	}
}
func (w *Workspace) traySummary() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	running, connected := 0, 0
	for _, e := range w.engines {
		e.mu.Lock()
		if e.state == "running" {
			running++
		}
		if e.current != "" {
			connected++
		}
		e.mu.Unlock()
	}
	return fmt.Sprintf("链跃 LinkWeaver · %d/%d 个出口已连接", connected, max(running, len(w.engines)))
}

func (w *Workspace) saveProfile(p ExitProfile) error {
	w.op.Lock()
	defer w.op.Unlock()
	c := w.snapshot()
	idx := -1
	for i, x := range c.Profiles {
		if x.ID == p.ID {
			idx = i
		}
	}
	if idx < 0 {
		p.ID = randomToken()[:12]
		for slot := 0; slot < 500; slot++ {
			verify, control := 31000+slot*2, 31001+slot*2
			used := false
			for _, x := range c.Profiles {
				if x.VerifyPort == verify || x.ControllerPort == control || x.ProxyPort == verify || x.ProxyPort == control {
					used = true
				}
			}
			if !used && p.ProxyPort != verify && p.ProxyPort != control {
				p.VerifyPort = verify
				p.ControllerPort = control
				break
			}
		}
	} else {
		old := c.Profiles[idx]
		p.VerifyPort = old.VerifyPort
		p.ControllerPort = old.ControllerPort
		p.Running = old.Running
		p.SelectionMode = old.SelectionMode
		p.ManualRelayID = old.ManualRelayID
		p.FixedExit = mergeProxy(old.FixedExit, p.FixedExit)
	}
	if p.ProxyPort == 0 {
		p.ProxyPort = 27892 + len(c.Profiles)*2
	}
	if idx < 0 {
		c.Profiles = append(c.Profiles, p)
	} else {
		c.Profiles[idx] = p
	}
	return w.commit(c)
}
func (w *Workspace) deleteProfile(id string) error {
	w.op.Lock()
	defer w.op.Unlock()
	c := w.snapshot()
	out := []ExitProfile{}
	found := false
	for _, p := range c.Profiles {
		if p.ID == id {
			found = true
			continue
		}
		out = append(out, p)
	}
	if !found {
		return errors.New("出口不存在")
	}
	c.Profiles = out
	return w.commit(c)
}

func proxyID(p map[string]any) string {
	q := cloneMap(p)
	delete(q, "name")
	b, _ := json.Marshal(q)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
func (w *Workspace) importRelays(text, sourceID string) (int, error) {
	nodes, err := parseProxyText(text)
	if err != nil {
		return 0, err
	}
	w.op.Lock()
	defer w.op.Unlock()
	c := w.snapshot()
	if sourceID == "" {
		sourceID = "manual"
	}
	sourceExists := false
	for _, s := range c.Sources {
		if s.ID == sourceID {
			sourceExists = true
		}
	}
	if !sourceExists {
		c.Sources = append(c.Sources, RelaySource{ID: sourceID, Name: "手动添加", Enabled: true, Priority: 100})
	}
	count := 0
	for _, p := range nodes {
		key := proxyID(p)
		c.DeletedNodes = slices.DeleteFunc(c.DeletedNodes, func(x string) bool { return x == key })
		exists := false
		for i := range c.Nodes {
			if proxyID(c.Nodes[i].Proxy) == key {
				ids := nodeSources(c.Nodes[i])
				if !slices.Contains(ids, sourceID) {
					ids = append(ids, sourceID)
				}
				setNodeSources(&c.Nodes[i], ids)
				c.Nodes[i].Retired = false
				exists = true
				break
			}
		}
		if exists {
			continue
		}
		label, _ := p["name"].(string)
		if label == "" {
			label = "手动线路 " + fmt.Sprint(len(c.Nodes)+1)
		}
		delete(p, "name")
		c.Nodes = append(c.Nodes, Node{ID: key, Label: label, Proxy: p, SourceID: sourceID, SourceIDs: []string{sourceID}, Priority: 100})
		count++
	}
	return count, w.commit(c)
}
func (w *Workspace) saveNode(n Node) error {
	w.op.Lock()
	defer w.op.Unlock()
	c := w.snapshot()
	found := false
	for i, old := range c.Nodes {
		if old.ID == n.ID {
			n.SourceID = old.SourceID
			n.SourceIDs = old.SourceIDs
			n.Retired = old.Retired
			n.Proxy = mergeProxy(old.Proxy, n.Proxy)
			n.SeedAvailable = false
			c.Nodes[i] = n
			found = true
			break
		}
	}
	if !found {
		return errors.New("中转节点不存在")
	}
	if n.Priority < 1 || n.Priority > 10000 {
		return errors.New("节点优先级范围 1–10000")
	}
	return w.commit(c)
}
func (w *Workspace) deleteNode(id string) error {
	_, err := w.batchNodes(nodeBatchRequest{Action: "delete", IDs: []string{id}})
	return err
}

func (w *Workspace) saveSource(s RelaySource) error {
	w.op.Lock()
	defer w.op.Unlock()
	c := w.snapshot()
	found := false
	for i, old := range c.Sources {
		if old.ID == s.ID {
			if s.URL == "" {
				s.URL = old.URL
			}
			s.UpdatedAt = old.UpdatedAt
			s.LastError = old.LastError
			for j := range c.Nodes {
				if c.Nodes[j].SourceID == s.ID && nodePriority(c.Nodes[j]) == old.Priority {
					c.Nodes[j].Priority = s.Priority
				}
			}
			c.Sources[i] = s
			found = true
			break
		}
	}
	if !found {
		s.ID = randomToken()[:12]
		c.Sources = append(c.Sources, s)
	}
	if s.UserAgent == "" {
		s.UserAgent = "Clash.Meta"
	}
	for i := range c.Sources {
		if c.Sources[i].ID == s.ID {
			c.Sources[i].UserAgent = s.UserAgent
		}
	}
	return w.commit(c)
}
func (w *Workspace) deleteSource(id string) error {
	w.op.Lock()
	defer w.op.Unlock()
	c := w.snapshot()
	ss := []RelaySource{}
	for _, s := range c.Sources {
		if s.ID != id {
			ss = append(ss, s)
		}
	}
	nn := []Node{}
	for _, n := range c.Nodes {
		ids := slices.DeleteFunc(slices.Clone(nodeSources(n)), func(x string) bool { return x == id })
		if len(ids) > 0 {
			setNodeSources(&n, ids)
			nn = append(nn, n)
		}
	}
	// Keep an explicitly selected deleted source as an empty pool, instead of
	// silently changing this exit to use every other subscription.
	c.Nodes = nn
	c.Sources = ss
	return w.commit(c)
}

var secretKeys = map[string]bool{"password": true, "username": true, "uuid": true, "obfs-password": true, "private-key": true, "token": true, "psk": true, "authorization": true}

func secretKey(k string) bool { return secretKeys[strings.ToLower(k)] }
func safeValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			if !secretKey(k) {
				out[k] = safeValue(v)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = safeValue(v)
		}
		return out
	default:
		return v
	}
}
func safeProxy(p map[string]any) map[string]any {
	if p == nil {
		return nil
	}
	return safeValue(cloneMap(p)).(map[string]any)
}
func clearSecrets(p map[string]any) {
	for k, v := range p {
		if secretKey(k) {
			delete(p, k)
			continue
		}
		if m, ok := v.(map[string]any); ok {
			clearSecrets(m)
		}
	}
}
func mergeProxy(old, updated map[string]any) map[string]any {
	out := cloneMap(old)
	if out == nil {
		out = map[string]any{}
	}
	oldType, _ := out["type"].(string)
	newType, _ := updated["type"].(string)
	if newType != "" && oldType != newType {
		out = map[string]any{}
	}
	if updated["_clearAuth"] == true {
		clearSecrets(out)
	}
	mergeFields(out, updated)
	delete(out, "_clearAuth")
	delete(out, "name")
	delete(out, "dialer-proxy")
	return out
}
func mergeFields(out, updated map[string]any) {
	for k, v := range updated {
		if k == "_clearAuth" {
			continue
		}
		if secretKey(k) && v == "" {
			continue
		}
		if v == nil {
			delete(out, k)
			continue
		}
		if nested, ok := v.(map[string]any); ok {
			base, _ := out[k].(map[string]any)
			if base == nil {
				base = map[string]any{}
			}
			mergeFields(base, nested)
			out[k] = base
		} else {
			out[k] = v
		}
	}
}
