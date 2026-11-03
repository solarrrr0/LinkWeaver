package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Microsoft/go-winio"
	"go.yaml.in/yaml/v3"
)

const clashBlockBegin = "// LINKWEAVER MANAGED BEGIN"
const clashBlockEnd = "// LINKWEAVER MANAGED END"

type clashAdapter struct {
	root, script string
	runtime      []byte
	document     map[string]any
	secret       string
	client       *http.Client
	base         string
}
type clashIntegration struct {
	Script    string    `json:"script"`
	Names     []string  `json:"names"`
	UpdatedAt time.Time `json:"updatedAt"`
	Backup    string    `json:"backup"`
}

func readYAMLFile(path string) (map[string]any, []byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var d map[string]any
	if yaml.Unmarshal(b, &d) != nil || d == nil {
		return nil, nil, errors.New("配置格式无效")
	}
	return d, b, nil
}
func discoverVerge(root string) (*clashAdapter, error) {
	if root == "" {
		root = filepath.Join(os.Getenv("APPDATA"), "io.github.clash-verge-rev.clash-verge-rev")
	}
	profiles, _, err := readYAMLFile(filepath.Join(root, "profiles.yaml"))
	if err != nil {
		return nil, errors.New("没有找到 Clash Verge Rev 的订阅配置")
	}
	current, _ := profiles["current"].(string)
	items, _ := profiles["items"].([]any)
	scriptID := ""
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["uid"] == current {
			opts, _ := item["option"].(map[string]any)
			scriptID, _ = opts["script"].(string)
		}
	}
	if scriptID == "" {
		return nil, errors.New("当前 Clash 订阅没有局部增强脚本，请先在 Clash 为当前订阅创建脚本")
	}
	scriptFile := ""
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["uid"] == scriptID && item["type"] == "script" {
			scriptFile, _ = item["file"].(string)
		}
	}
	if scriptFile == "" || filepath.Base(scriptFile) != scriptFile || !strings.HasSuffix(scriptFile, ".js") {
		return nil, errors.New("Clash 局部增强脚本路径无效")
	}
	script := filepath.Join(root, "profiles", scriptFile)
	b, err := os.ReadFile(script)
	if err != nil || !strings.Contains(string(b), "main") {
		return nil, errors.New("无法读取当前订阅增强脚本")
	}
	doc, runtime, err := readYAMLFile(filepath.Join(root, "clash-verge.yaml"))
	if err != nil {
		return nil, errors.New("Clash 运行配置尚未生成，请先启动客户端并选择订阅")
	}
	secret, _ := doc["secret"].(string)
	return &clashAdapter{root: root, script: script, runtime: runtime, document: doc, secret: secret}, nil
}
func enumerateVergePipes() []string {
	var data syscall.Win32finddata
	handle, err := syscall.FindFirstFile(syscall.StringToUTF16Ptr(`\\.\pipe\*`), &data)
	if err != nil {
		return nil
	}
	defer syscall.FindClose(handle)
	out := []string{}
	for {
		name := syscall.UTF16ToString(data.FileName[:])
		if strings.HasPrefix(name, "verge-mihomo-") {
			out = append(out, `\\.\pipe\`+name)
		}
		if syscall.FindNextFile(handle, &data) != nil {
			break
		}
	}
	return out
}
func (a *clashAdapter) connect(ctx context.Context) error {
	pipes := []string{}
	if pipe, ok := a.document["external-controller-pipe"].(string); ok && strings.HasPrefix(pipe, `\\.\pipe\verge-mihomo-`) {
		pipes = append(pipes, pipe)
	}
	for _, p := range enumerateVergePipes() {
		found := false
		for _, x := range pipes {
			if x == p {
				found = true
			}
		}
		if !found {
			pipes = append(pipes, p)
		}
	}
	denied := false
	unauthorized := false
	for _, pipe := range pipes {
		p := pipe
		transport := &http.Transport{DialContext: func(c context.Context, _, _ string) (net.Conn, error) { return winio.DialPipeContext(c, p) }}
		a.client = &http.Client{Transport: transport, Timeout: 4 * time.Second}
		a.base = "http://clash.local"
		check, cancel := context.WithTimeout(ctx, 1400*time.Millisecond)
		_, err := a.call(check, "GET", "/version", nil)
		cancel()
		if err == nil {
			a.client.Timeout = 15 * time.Second
			return nil
		}
		transport.CloseIdleConnections()
		if strings.Contains(err.Error(), "401") {
			unauthorized = true
		}
		if strings.Contains(strings.ToLower(err.Error()), "denied") {
			denied = true
		}
	}
	addresses := []string{}
	if address, ok := a.document["external-controller"].(string); ok {
		addresses = append(addresses, address)
	}
	if base, _, err := readYAMLFile(filepath.Join(a.root, "config.yaml")); err == nil {
		if address, ok := base["external-controller"].(string); ok {
			addresses = append(addresses, address)
		}
	}
	for _, address := range addresses {
		host, _, err := net.SplitHostPort(address)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			continue
		}
		a.client = &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 4 * time.Second}
		a.base = "http://" + address
		check, cancel := context.WithTimeout(ctx, time.Second)
		_, err = a.call(check, "GET", "/version", nil)
		cancel()
		if err == nil {
			a.client.Timeout = 15 * time.Second
			return nil
		}
		a.client.CloseIdleConnections()
		if strings.Contains(err.Error(), "401") {
			unauthorized = true
		}
	}
	if unauthorized {
		return errors.New("Clash 控制接口认证失败，未修改配置")
	}
	if denied {
		return errors.New("Clash 控制管道权限不足，未修改配置")
	}
	return errors.New("未连接到运行中的 Clash 控制接口，请先启动 Clash Verge Rev 并选择原订阅")
}
func (a *clashAdapter) call(ctx context.Context, method, path string, body any) ([]byte, error) {
	var data io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		data = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(ctx, method, a.base+path, data)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+a.secret)
	r.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Clash HTTP %d", resp.StatusCode)
	}
	return b, nil
}
func managedClashName(name string) bool { return strings.HasPrefix(name, "链跃 · ") }
func mergeClashDocument(doc map[string]any, entries []ClashEntrance) (map[string]any, int, error) {
	out := cloneMap(doc)
	proxies, _ := out["proxies"].([]any)
	clean := []any{}
	for _, raw := range proxies {
		p, _ := raw.(map[string]any)
		name, _ := p["name"].(string)
		if !managedClashName(name) {
			clean = append(clean, raw)
		}
	}
	names := []string{}
	for _, p := range entries {
		clean = append(clean, map[string]any{"name": p.Name, "type": "socks5", "server": "127.0.0.1", "port": p.Port, "udp": false})
		names = append(names, p.Name)
	}
	out["proxies"] = clean
	count := 0
	groups, _ := out["proxy-groups"].([]any)
	for _, raw := range groups {
		g, _ := raw.(map[string]any)
		if g["type"] != "select" {
			continue
		}
		members, _ := g["proxies"].([]any)
		next := []any{}
		for _, x := range members {
			if name, ok := x.(string); ok && !managedClashName(name) {
				next = append(next, name)
			}
		}
		for _, name := range names {
			next = append(next, name)
		}
		g["proxies"] = next
		count++
	}
	if count == 0 {
		return nil, 0, errors.New("当前 Clash 配置没有可追加的手动选择组")
	}
	return out, count, nil
}
func stripClashBlock(script string) (string, error) {
	i := strings.Index(script, clashBlockBegin)
	j := strings.Index(script, clashBlockEnd)
	if i < 0 && j < 0 {
		return script, nil
	}
	if i < 0 || j < i {
		return "", errors.New("现有链跃增强脚本标记不完整，请先检查脚本")
	}
	return strings.TrimRight(script[:i]+script[j+len(clashBlockEnd):], "\r\n"), nil
}
func injectClashScript(script string, entries []ClashEntrance) (string, error) {
	base, err := stripClashBlock(script)
	if err != nil {
		return "", err
	}
	payload := []map[string]any{}
	for _, p := range entries {
		payload = append(payload, map[string]any{"name": p.Name, "type": "socks5", "server": "127.0.0.1", "port": p.Port, "udp": false})
	}
	b, _ := json.Marshal(payload)
	block := clashBlockBegin + "\nconst __linkWeaverPreviousMain = main;\nmain = function(config, profileName) {\n  const result = __linkWeaverPreviousMain(config, profileName) || config;\n  const own = name => typeof name === 'string' && name.startsWith('链跃 · ');\n  const entries = " + string(b) + ";\n  result.proxies = (result.proxies || []).filter(p => !own(p.name)).concat(entries);\n  for (const group of (result['proxy-groups'] || [])) {\n    if (group.type === 'select') group.proxies = (group.proxies || []).filter(p => !own(p)).concat(entries.map(p => p.name));\n  }\n  return result;\n};\n" + clashBlockEnd + "\n"
	return base + "\n\n" + block, nil
}
func (a *clashAdapter) selections(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	b, err := a.call(ctx, "GET", "/proxies", nil)
	if err != nil {
		return nil, errors.New("无法读取 Clash 当前节点选择，未修改客户端")
	}
	var d struct {
		Proxies map[string]struct {
			Type string `json:"type"`
			Now  string `json:"now"`
		} `json:"proxies"`
	}
	if json.Unmarshal(b, &d) != nil {
		return nil, errors.New("Clash 节点选择响应无效，未修改客户端")
	}
	for name, p := range d.Proxies {
		if p.Type == "Selector" {
			out[name] = p.Now
		}
	}
	return out, nil
}
func (a *clashAdapter) restoreSelections(ctx context.Context, selections map[string]string) error {
	for group, name := range selections {
		if _, err := a.call(ctx, "PUT", "/proxies/"+url.PathEscape(group), map[string]string{"name": name}); err != nil {
			return err
		}
	}
	actual, err := a.selections(ctx)
	if err != nil {
		return err
	}
	for group, name := range selections {
		if actual[group] != name {
			return errors.New("节点选择未恢复")
		}
	}
	return nil
}
func (w *Workspace) integrateClash(ctx context.Context, entries []ClashEntrance, adapter *clashAdapter) (int, error) {
	if err := adapter.connect(ctx); err != nil {
		return 0, err
	}
	defer adapter.client.CloseIdleConnections()
	selections, err := adapter.selections(ctx)
	if err != nil {
		return 0, err
	}
	// Preserve the current runtime mode even if the generated file predates a UI change.
	if live, err := adapter.call(ctx, "GET", "/configs", nil); err == nil {
		var state map[string]any
		if json.Unmarshal(live, &state) == nil {
			if mode, ok := state["mode"].(string); ok {
				adapter.document["mode"] = mode
			}
		}
	}
	candidate, count, err := mergeClashDocument(adapter.document, entries)
	if err != nil {
		return 0, err
	}
	b, err := yaml.Marshal(candidate)
	if err != nil {
		return 0, errors.New("无法生成 Clash 配置")
	}
	originalScript, err := os.ReadFile(adapter.script)
	if err != nil {
		return 0, errors.New("无法读取 Clash 脚本")
	}
	updated, err := injectClashScript(string(originalScript), entries)
	if err != nil {
		return 0, err
	}
	// Validate the complete candidate with our compatible Mihomo before any writes.
	dir := filepath.Join(w.root, "runtime", "clash-integration")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return 0, err
	}
	f, err := os.CreateTemp(dir, "candidate-*.yaml")
	if err != nil {
		return 0, err
	}
	path := f.Name()
	_, err = f.Write(b)
	f.Close()
	defer os.Remove(path)
	if err != nil {
		return 0, err
	}
	// Resolve existing rule providers and geodata against the client's data directory.
	check := exec.CommandContext(ctx, filepath.Join(w.root, "bin", "mihomo.exe"), "-t", "-d", adapter.root, "-f", path)
	hideWindow(check)
	if check.Run() != nil {
		return 0, errors.New("Clash 合并配置未通过校验，未修改客户端")
	}
	backup := filepath.Join(w.root, "data", "backups", "clash-"+time.Now().Format("20060102-150405")+"-"+randomToken()[:6])
	if err = os.MkdirAll(backup, 0700); err != nil {
		return 0, err
	}
	if err = atomicWrite(filepath.Join(backup, "script.js"), originalScript); err != nil {
		return 0, err
	}
	if err = atomicWrite(filepath.Join(backup, "runtime.yaml"), adapter.runtime); err != nil {
		return 0, err
	}
	// Refuse to replace a script that changed during validation.
	current, err := os.ReadFile(adapter.script)
	if err != nil || !bytes.Equal(current, originalScript) {
		return 0, errors.New("Clash 脚本已被其他程序修改，请重试")
	}
	if err = atomicWrite(adapter.script, []byte(updated)); err != nil {
		return 0, errors.New("无法保存 Clash 局部增强脚本")
	}
	_, err = adapter.call(ctx, "PUT", "/configs?force=true", map[string]string{"payload": string(b)})
	if err == nil {
		err = adapter.restoreSelections(ctx, selections)
	}
	if err != nil {
		scriptErr := atomicWrite(adapter.script, originalScript)
		rollback, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_, coreErr := adapter.call(rollback, "PUT", "/configs?force=true", map[string]string{"payload": string(adapter.runtime)})
		selectionErr := adapter.restoreSelections(rollback, selections)
		if scriptErr != nil || coreErr != nil || selectionErr != nil {
			return 0, errors.New("Clash 接入失败且自动回滚未完全成功，请从 data/backups 的 Clash 备份恢复")
		}
		return 0, errors.New("Clash 重载或选择恢复失败，已回滚原脚本和运行配置")
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name)
	}
	state := clashIntegration{Script: adapter.script, Names: names, UpdatedAt: time.Now(), Backup: backup}
	stateBytes, _ := json.Marshal(state)
	if err = atomicWrite(filepath.Join(w.root, "data", "clash-integration.json"), stateBytes); err != nil {
		return count, errors.New("Clash 接入已应用，但接入记录保存失败；脚本备份在 data/backups")
	}
	return count, nil
}

func (s *Server) clashIntegrationStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.WriteHeader(405)
		return
	}
	adapter, err := discoverVerge("")
	if err != nil {
		respond(w, map[string]any{"available": false, "message": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	err = adapter.connect(ctx)
	if adapter.client != nil {
		adapter.client.CloseIdleConnections()
	}
	message := "可接入当前 Clash 订阅，保留原分流规则、DNS 和节点选择"
	if err != nil {
		message = err.Error()
	}
	respond(w, map[string]any{"available": err == nil, "message": message, "scriptConfigured": true})
}
func (s *Server) clashIntegrate(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	var v struct {
		Profile string `json:"profile"`
	}
	if err := decode(w, r, &v); err != nil {
		fail(w, err)
		return
	}
	entries, err := s.clashEntrances(v.Profile)
	if err != nil {
		fail(w, err)
		return
	}
	// Native integration manages all ready local exits so a per-card action cannot remove another exit.
	entries, err = s.clashEntrances("")
	if err != nil {
		fail(w, err)
		return
	}
	adapter, err := discoverVerge("")
	if err != nil {
		fail(w, err)
		return
	}
	s.workspace.op.Lock()
	defer s.workspace.op.Unlock()
	count, err := s.workspace.integrateClash(r.Context(), entries, adapter)
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]any{"ok": true, "entranceCount": len(entries), "groupCount": count})
}
