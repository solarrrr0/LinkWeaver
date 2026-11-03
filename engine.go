package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

type Event struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}
type Timing struct {
	At          time.Time `json:"at"`
	TotalMS     float64   `json:"totalMS"`
	ConnectMS   float64   `json:"connectMS"`
	TLSMS       float64   `json:"tlsMS"`
	FirstByteMS float64   `json:"firstByteMS"`
	HTTPStatus  int       `json:"httpStatus"`
	OK          bool      `json:"ok"`
}
type Engine struct {
	mu         sync.Mutex
	lifecycle  sync.Mutex
	scanMu     sync.Mutex
	verifyMu   sync.Mutex
	root       string
	scope      string
	config     Config
	routes     map[string]*Route
	state      string
	current    string
	pinned     string
	lastSwitch time.Time
	events     []Event
	timing     *Timing
	observedIP string
	secret     string
	cancel     context.CancelFunc
	runContext context.Context
	cmd        *exec.Cmd
	done       chan struct{}
	scanning   bool
	checked    int
	total      int
	lastScan   time.Time
	automatic  bool
	lastError  string
	client     *http.Client
}

func newEngine(root string, c Config) *Engine {
	e := &Engine{root: root, config: c, state: "stopped", routes: map[string]*Route{}, automatic: true, client: &http.Client{Timeout: 18 * time.Second, Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 8}}}
	e.resetRoutes()
	return e
}
func (e *Engine) resetRoutes() {
	e.routes = map[string]*Route{}
	for _, n := range e.config.Nodes {
		p, _ := n.Proxy["type"].(string)
		e.routes[n.ID] = &Route{ID: n.ID, Label: n.Label, Protocol: p, Status: "unknown", Samples: []Sample{}, SeedAvailable: n.SeedAvailable, Priority: nodePriority(n), GroupPriority: n.GroupPriority, GroupName: n.GroupName, Paused: n.Paused}
	}
}
func (e *Engine) event(text string) { e.mu.Lock(); defer e.mu.Unlock(); e.eventLocked(text) }
func (e *Engine) eventLocked(text string) {
	e.events = append(e.events, Event{time.Now(), text})
	if len(e.events) > 80 {
		e.events = e.events[len(e.events)-80:]
	}
}
func (e *Engine) cfg() Config { e.mu.Lock(); defer e.mu.Unlock(); return e.config }
func (e *Engine) call(ctx context.Context, method, path string, body any) ([]byte, error) {
	e.mu.Lock()
	port, secret := e.config.ControllerPort, e.secret
	e.mu.Unlock()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	r, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), rd)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+secret)
	r.Header.Set("Content-Type", "application/json")
	res, err := e.client.Do(r)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1024*1024))
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("core HTTP %d", res.StatusCode)
	}
	return b, nil
}
func (e *Engine) selectGroup(ctx context.Context, group, id string) error {
	name := "REJECT"
	if id != "" {
		name = "chain-" + id
	}
	_, err := e.call(ctx, http.MethodPut, "/proxies/"+group, map[string]string{"name": name})
	return err
}

func portFree(port int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func (e *Engine) Start() error {
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()
	e.mu.Lock()
	if e.state != "stopped" && e.state != "error" {
		e.mu.Unlock()
		return errors.New("核心已启动或正在启动")
	}
	c := e.config
	e.mu.Unlock()
	if len(c.Nodes) == 0 {
		return errors.New("请先导入节点和固定出口")
	}
	if err := validateProxy(c.FixedExit); err != nil && !c.ProbeOnly {
		return errors.New("请先配置固定出口")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	ports := []int{c.ProxyPort, c.VerifyPort, c.ControllerPort}
	if c.ProbeOnly {
		ports = []int{c.ControllerPort}
	}
	for _, p := range ports {
		if !portFree(p) {
			return fmt.Errorf("本地端口 %d 被其他程序占用", p)
		}
	}
	core := filepath.Join(e.root, "bin", "mihomo.exe")
	if _, err := os.Stat(core); err != nil {
		return errors.New("缺少 bin/mihomo.exe")
	}
	runtimeDir := filepath.Join(e.root, "runtime")
	if e.scope != "" {
		runtimeDir = filepath.Join(runtimeDir, e.scope)
	}
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		return err
	}
	secret := randomToken()
	b, _ := json.MarshalIndent(buildCoreConfig(c, secret), "", "  ")
	path := filepath.Join(runtimeDir, "core.json")
	if err := atomicWrite(path, b); err != nil {
		return err
	}
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer checkCancel()
	check := exec.CommandContext(checkCtx, core, "-t", "-d", runtimeDir, "-f", path)
	hideWindow(check)
	if err := check.Run(); err != nil {
		return errors.New("核心配置校验失败；请检查导入的协议和字段")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, core, "-d", runtimeDir, "-f", path)
	hideWindow(cmd)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		return errors.New("无法启动 Mihomo 核心")
	}
	e.mu.Lock()
	e.secret = secret
	e.state = "starting"
	e.current = ""
	e.pinned = c.ManualRelayID
	e.automatic = c.SelectionMode != "manual"
	e.observedIP = ""
	e.lastError = ""
	e.cancel = cancel
	e.runContext = ctx
	e.cmd = cmd
	e.done = make(chan struct{})
	done := e.done
	e.resetRoutes()
	e.mu.Unlock()
	go func() {
		_ = cmd.Wait()
		e.mu.Lock()
		unexpected := ctx.Err() == nil
		if unexpected {
			e.state = "error"
			e.lastError = "代理核心意外退出"
			cancel()
		}
		e.mu.Unlock()
		close(done)
	}()
	ready := false
	for i := 0; i < 40; i++ {
		select {
		case <-done:
			cancel()
			return errors.New("核心在启动期间退出")
		default:
		}
		q, qcancel := context.WithTimeout(ctx, 400*time.Millisecond)
		_, err := e.call(q, http.MethodGet, "/version", nil)
		qcancel()
		if err == nil {
			ready = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		cancel()
		<-done
		e.mu.Lock()
		e.state = "error"
		e.lastError = "核心控制接口启动超时"
		e.mu.Unlock()
		return errors.New("核心控制接口启动超时")
	}
	e.mu.Lock()
	e.state = "running"
	e.mu.Unlock()
	e.event("核心已启动；入口暂时阻断，等待完整链路和出口身份验证")
	if !c.ProbeOnly {
		go e.loop(ctx)
	}
	return nil
}

func (e *Engine) Stop() {
	e.lifecycle.Lock()
	defer e.lifecycle.Unlock()
	e.mu.Lock()
	cancel, done := e.cancel, e.done
	e.cancel = nil
	e.state = "stopping"
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	e.scanMu.Lock()
	e.scanMu.Unlock()
	e.verifyMu.Lock()
	e.verifyMu.Unlock()
	e.mu.Lock()
	e.state = "stopped"
	e.current = ""
	e.scanning = false
	e.cmd = nil
	e.runContext = nil
	e.mu.Unlock()
	e.event("代理已停止")
}

func (e *Engine) loop(ctx context.Context) {
	c := e.cfg()
	if c.SelectionMode == "manual" && c.ManualRelayID != "" {
		if err := e.ManualSelect(c.ManualRelayID); err != nil {
			e.event("手动锁定中转恢复失败；保持锁定，不改用其他节点")
		}
	}
	_ = e.Scan(ctx, true)
	for {
		c := e.cfg()
		timer := time.NewTimer(time.Duration(c.IntervalSeconds) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		_ = e.Scan(ctx, false)
	}
}

type probeJob struct {
	id     string
	target string
}

func (e *Engine) probe(ctx context.Context, id, target string) Sample {
	c := e.cfg()
	q := url.Values{"url": {target}, "timeout": {fmt.Sprint(c.TimeoutMS)}, "expected": {"200-399"}}
	b, err := e.call(ctx, http.MethodGet, "/proxies/chain-"+url.PathEscape(id)+"/delay?"+q.Encode(), nil)
	var d struct {
		Delay int `json:"delay"`
	}
	if err == nil {
		err = json.Unmarshal(b, &d)
	}
	return Sample{At: time.Now(), MS: float64(d.Delay), OK: err == nil && d.Delay > 0, Target: target}
}

func (e *Engine) Scan(ctx context.Context, full bool) error {
	return e.scan(ctx, full, nil)
}

func (e *Engine) scan(ctx context.Context, full bool, only []string) error {
	if !e.scanMu.TryLock() {
		return errors.New("已有检测任务正在运行")
	}
	defer e.scanMu.Unlock()
	e.mu.Lock()
	if e.state != "running" {
		e.mu.Unlock()
		return errors.New("请先启动核心")
	}
	c := e.config
	current := e.current
	ids := []string{}
	now := time.Now()
	for id, r := range e.routes {
		if len(only) > 0 && !slices.Contains(only, id) {
			continue
		}
		if full || id == current || !now.Before(r.RetryAfter) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := e.routes[ids[i]], e.routes[ids[j]]
		if a.ID == b.ID {
			return false
		}
		if a.ID == current {
			return true
		}
		if b.ID == current {
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
		if a.SeedAvailable != b.SeedAvailable {
			return a.SeedAvailable
		}
		if a.Score != b.Score && a.Score > 0 && b.Score > 0 {
			return a.Score < b.Score
		}
		return a.ID < b.ID
	})
	// A regular round rechecks the active route, best alternatives and a rotating set of unexplored routes.
	if !full && len(ids) > 10 {
		best := append([]string{}, ids[:6]...)
		rest := ids[6:]
		offset := int(now.Unix()/int64(c.IntervalSeconds)) % len(rest)
		for i := 0; i < 4; i++ {
			best = append(best, rest[(offset+i)%len(rest)])
		}
		ids = best
	}
	e.scanning = true
	e.checked = 0
	e.total = len(ids)
	e.mu.Unlock()
	defer func() { e.mu.Lock(); e.scanning = false; e.lastScan = time.Now(); e.mu.Unlock() }()
	jobs := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < c.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				if ctx.Err() != nil {
					return
				}
				s := e.probe(ctx, id, c.Targets[0])
				if ctx.Err() != nil {
					return
				}
				e.mu.Lock()
				r := e.routes[id]
				r.Record(s)
				e.checked++
				noCurrent := e.current == ""
				e.mu.Unlock()
				if s.OK && noCurrent && !c.ProbeOnly {
					_ = e.trySelect(ctx, id, "首次完整链路可用", false)
				}
			}
		}()
	}
	for _, id := range ids {
		select {
		case jobs <- id:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if c.ProbeOnly {
		_ = e.persistMetrics()
		return nil
	}
	// Confirm short-listed routes with a fresh primary request and secondary HTTPS origins.
	// Recheck the active route so a lengthy full scan cannot leave its comparison sample stale.
	e.mu.Lock()
	rs := ranked(e.routes, time.Now(), c)
	top := []string{}
	for _, r := range rs[:min(3, len(rs))] {
		top = append(top, r.ID)
	}
	active := e.current
	found := false
	for _, id := range top {
		if id == active {
			found = true
		}
	}
	if active != "" && !found {
		top = append(top, active)
	}
	firstScan := e.lastScan.IsZero()
	e.mu.Unlock()
	for _, id := range top {
		s := e.probe(ctx, id, c.Targets[0])
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.mu.Lock()
		e.routes[id].Record(s)
		e.mu.Unlock()
		if !s.OK {
			continue
		}
		for _, target := range c.Targets[1:] {
			s = e.probe(ctx, id, target)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			e.mu.Lock()
			r := e.routes[id]
			streak := r.ConsecutiveSuccesses
			r.Record(s)
			if s.OK {
				r.ConsecutiveSuccesses = streak
			}
			e.mu.Unlock()
		}
	}
	e.mu.Lock()
	switchTime := e.lastSwitch
	if firstScan {
		switchTime = time.Time{}
	}
	id, reason := choose(e.routes, e.current, switchTime, time.Now(), c)
	automatic := e.automatic
	pinned := e.pinned
	cur := e.routes[e.current]
	manualNeedsRestore := e.current == "" && pinned != "" && e.routes[pinned] != nil && e.routes[pinned].ConsecutiveFailures == 0 && e.routes[pinned].Successes > 0
	failed := cur != nil && cur.ConsecutiveFailures >= 2
	e.mu.Unlock()
	if !automatic || pinned != "" {
		if manualNeedsRestore {
			if err := e.trySelect(ctx, pinned, "手动锁定中转重新可达，恢复原线路", true); err != nil {
				e.event("锁定中转尚未通过出口校验，继续等待")
			}
		}
		if failed {
			e.event("手动锁定线路检测失败：只告警，保持当前转发，不自动切换")
		}
		_ = e.persistMetrics()
		return nil
	}

	if id != "" {
		if err := e.trySelect(ctx, id, reason, false); err != nil {
			e.event("候选线路出口验证未通过，保持原有策略")
			e.mu.Lock()
			r := e.routes[id]
			if r != nil {
				r.Record(Sample{At: time.Now(), OK: false, Target: "出口身份验证"})
			}
			e.mu.Unlock()
			if failed {
				_ = e.selectGroup(ctx, "ROUTE", "")
				e.mu.Lock()
				e.current = ""
				e.observedIP = ""
				e.mu.Unlock()
			}
		}
	} else if failed {
		_ = e.selectGroup(ctx, "ROUTE", "")
		e.mu.Lock()
		e.current = ""
		e.observedIP = ""
		e.mu.Unlock()
		e.event("没有已验证的备用链路，已阻断新连接")
	} else {
		e.mu.Lock()
		empty := e.current == ""
		e.mu.Unlock()
		if empty {
			e.event("本轮没有通过完整链路验证的候选，保持入口阻断")
		}
	}
	_ = e.persistMetrics()
	return nil
}

func (e *Engine) runCtx() (context.Context, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state != "running" || e.runContext == nil {
		return nil, errors.New("核心未运行")
	}
	return e.runContext, nil
}
func (e *Engine) RequestScan() error {
	ctx, err := e.runCtx()
	if err != nil {
		return err
	}
	e.mu.Lock()
	busy := e.scanning
	e.mu.Unlock()
	if busy {
		return errors.New("检测正在进行中")
	}
	go func() { _ = e.Scan(ctx, true) }()
	return nil
}

func proxyClient(port, timeout int) *http.Client {
	u, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	return &http.Client{Timeout: time.Duration(timeout) * time.Millisecond, Transport: &http.Transport{Proxy: http.ProxyURL(u), DisableKeepAlives: true, TLSHandshakeTimeout: time.Duration(timeout) * time.Millisecond}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func (e *Engine) verifyIdentity(ctx context.Context, port int) (string, error) {
	c := e.cfg()
	client := proxyClient(port, max(8000, c.TimeoutMS))
	defer client.CloseIdleConnections()
	// Providers are contacted only through the candidate's fixed exit.
	for _, target := range []string{"https://api.ipify.org", "https://checkip.amazonaws.com"} {
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return "", err
		}
		res, err := client.Do(r)
		if err != nil {
			continue
		}
		b, readErr := io.ReadAll(io.LimitReader(res.Body, 128))
		res.Body.Close()
		if readErr != nil || res.StatusCode != 200 {
			continue
		}
		ip := net.ParseIP(strings.TrimSpace(string(b)))
		if ip == nil {
			continue
		}
		if c.ExpectedIP != "" && !ip.Equal(net.ParseIP(c.ExpectedIP)) {
			return "", errors.New("出口身份不一致")
		}
		return ip.String(), nil
	}
	return "", errors.New("未能验证最终出口 IP")
}

func (e *Engine) trySelect(ctx context.Context, id, reason string, manual bool) error {
	e.verifyMu.Lock()
	defer e.verifyMu.Unlock()
	e.mu.Lock()
	if e.state != "running" || e.routes[id] == nil {
		e.mu.Unlock()
		return errors.New("线路不存在或核心已停止")
	}
	if !manual && (e.routes[id].Paused || !e.automatic) {
		e.mu.Unlock()
		return nil
	}
	if !manual && reason == "首次完整链路可用" && e.current != "" {
		e.mu.Unlock()
		return nil
	}
	if e.current == id {
		if manual {
			e.pinned = id
		}
		e.mu.Unlock()
		return nil
	}
	if e.pinned != "" && !manual {
		e.mu.Unlock()
		return nil
	}
	e.mu.Unlock()
	if err := e.selectGroup(ctx, "VERIFY", id); err != nil {
		return errors.New("无法设置验证线路")
	}
	defer e.selectGroup(context.Background(), "VERIFY", "")
	c := e.cfg()
	client := proxyClient(c.VerifyPort, c.TimeoutMS)
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.Targets[0], nil)
	if err != nil {
		return errors.New("无效测试目标")
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("候选 HTTPS 请求失败")
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return errors.New("候选 HTTPS 响应状态异常")
	}
	ip, err := e.verifyIdentity(ctx, c.VerifyPort)
	if err != nil {
		return err
	}
	if err = e.selectGroup(ctx, "ROUTE", id); err != nil {
		return errors.New("无法应用线路选择")
	}
	e.mu.Lock()
	e.current = id
	e.observedIP = ip
	e.lastSwitch = time.Now()
	if manual {
		e.pinned = id
	}
	e.eventLocked(reason + "；出口身份已验证")
	e.mu.Unlock()
	return nil
}

func (e *Engine) ManualSelect(id string) error {
	ctx, err := e.runCtx()
	if err != nil {
		return err
	}
	if id == "" {
		e.mu.Lock()
		e.pinned = ""
		e.automatic = true
		e.mu.Unlock()
		e.event("已恢复自动选择")
		return nil
	}
	s := e.probe(ctx, id, e.cfg().Targets[0])
	e.mu.Lock()
	r := e.routes[id]
	if r != nil {
		r.Record(s)
	}
	e.mu.Unlock()
	if r == nil {
		return errors.New("线路不存在")
	}
	if !s.OK {
		return errors.New("该线路完整链路检测失败")
	}
	return e.trySelect(ctx, id, "手动锁定线路", true)
}

func (e *Engine) Measure() (*Timing, error) {
	ctx, err := e.runCtx()
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	active := e.current
	e.mu.Unlock()
	if active == "" {
		return nil, errors.New("等待可用链路")
	}
	c := e.cfg()
	t := &Timing{At: time.Now()}
	start := time.Now()
	var connectStart, tlsStart time.Time
	var lock sync.Mutex
	trace := &httptrace.ClientTrace{ConnectStart: func(string, string) { lock.Lock(); connectStart = time.Now(); lock.Unlock() }, ConnectDone: func(string, string, error) {
		lock.Lock()
		if !connectStart.IsZero() {
			t.ConnectMS = float64(time.Since(connectStart).Microseconds()) / 1000
		}
		lock.Unlock()
	}, TLSHandshakeStart: func() { lock.Lock(); tlsStart = time.Now(); lock.Unlock() }, TLSHandshakeDone: tlsDone(t, &lock, &tlsStart), GotFirstResponseByte: func() { lock.Lock(); t.FirstByteMS = float64(time.Since(start).Microseconds()) / 1000; lock.Unlock() }}
	r, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodHead, c.Targets[0], nil)
	if err != nil {
		return nil, err
	}
	client := proxyClient(c.ProxyPort, c.TimeoutMS)
	defer client.CloseIdleConnections()
	res, reqErr := client.Do(r)
	t.TotalMS = float64(time.Since(start).Microseconds()) / 1000
	if reqErr == nil {
		t.HTTPStatus = res.StatusCode
		t.OK = res.StatusCode >= 200 && res.StatusCode < 400
		res.Body.Close()
	}
	e.mu.Lock()
	e.timing = t
	e.mu.Unlock()
	if reqErr != nil {
		return t, errors.New("当前链路请求超时或握手失败")
	}
	return t, nil
}

func (e *Engine) persistMetrics() error {
	e.mu.Lock()
	v := map[string]any{"at": time.Now(), "routes": e.routes, "events": e.events}
	b, err := json.MarshalIndent(v, "", "  ")
	e.mu.Unlock()
	if err != nil {
		return err
	}
	name := "metrics.json"
	if e.scope != "" {
		name = "metrics-" + e.scope + ".json"
	}
	return atomicWrite(filepath.Join(e.root, "data", name), b)
}
