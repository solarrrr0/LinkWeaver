package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Node struct {
	ID            string         `json:"id" yaml:"id"`
	Label         string         `json:"label" yaml:"label"`
	Proxy         map[string]any `json:"proxy" yaml:"proxy"`
	SeedAvailable bool           `json:"seedAvailable,omitempty" yaml:"seedAvailable,omitempty"`
	SourceID      string         `json:"sourceID,omitempty" yaml:"sourceID,omitempty"`
	SourceIDs     []string       `json:"sourceIDs,omitempty" yaml:"sourceIDs,omitempty"`
	Retired       bool           `json:"retired,omitempty" yaml:"retired,omitempty"`
	GroupPriority int            `json:"groupPriority,omitempty" yaml:"groupPriority,omitempty"`
	GroupName     string         `json:"groupName,omitempty" yaml:"groupName,omitempty"`
	Paused        bool           `json:"paused,omitempty" yaml:"paused,omitempty"`
	Disabled      bool           `json:"disabled,omitempty" yaml:"disabled,omitempty"`
	Priority      int            `json:"priority,omitempty" yaml:"priority,omitempty"`
}

type Config struct {
	PoolMode        string         `json:"poolMode,omitempty" yaml:"poolMode,omitempty"`
	SpeedPriority   bool           `json:"speedPriority" yaml:"speedPriority"`
	UnifiedDelay    bool           `json:"unifiedDelay" yaml:"unifiedDelay"`
	RelayGroups     []RelayGroup   `json:"relayGroups,omitempty" yaml:"relayGroups,omitempty"`
	DeletedNodes    []string       `json:"deletedNodes,omitempty" yaml:"deletedNodes,omitempty"`
	AutoRecover     bool           `json:"autoRecover" yaml:"autoRecover"`
	SelectionMode   string         `json:"selectionMode,omitempty" yaml:"selectionMode,omitempty"`
	ManualRelayID   string         `json:"manualRelayID,omitempty" yaml:"manualRelayID,omitempty"`
	ProbeOnly       bool           `json:"-" yaml:"-"`
	Profiles        []ExitProfile  `json:"profiles,omitempty" yaml:"profiles,omitempty"`
	Sources         []RelaySource  `json:"sources,omitempty" yaml:"sources,omitempty"`
	AutoRefresh     bool           `json:"autoRefresh" yaml:"autoRefresh"`
	RefreshMinutes  int            `json:"refreshMinutes" yaml:"refreshMinutes"`
	OpenOnLaunch    bool           `json:"openOnLaunch" yaml:"openOnLaunch"`
	ResumeOnLaunch  bool           `json:"resumeOnLaunch" yaml:"resumeOnLaunch"`
	RunAtLogin      bool           `json:"runAtLogin" yaml:"runAtLogin"`
	Version         int            `json:"version" yaml:"version"`
	Name            string         `json:"name" yaml:"name"`
	ExpectedIP      string         `json:"expectedIP" yaml:"expectedIP"`
	FixedExit       map[string]any `json:"fixedExit" yaml:"fixedExit"`
	Nodes           []Node         `json:"nodes" yaml:"nodes"`
	Targets         []string       `json:"targets" yaml:"targets"`
	IntervalSeconds int            `json:"intervalSeconds" yaml:"intervalSeconds"`
	TimeoutMS       int            `json:"timeoutMS" yaml:"timeoutMS"`
	Concurrency     int            `json:"concurrency" yaml:"concurrency"`
	MinGainMS       int            `json:"minGainMS" yaml:"minGainMS"`
	MinGainPercent  int            `json:"minGainPercent" yaml:"minGainPercent"`
	MinDwellSeconds int            `json:"minDwellSeconds" yaml:"minDwellSeconds"`
	UIPort          int            `json:"uiPort" yaml:"uiPort"`
	ProxyPort       int            `json:"proxyPort" yaml:"proxyPort"`
	VerifyPort      int            `json:"verifyPort" yaml:"verifyPort"`
	ControllerPort  int            `json:"controllerPort" yaml:"controllerPort"`
}

func defaults() Config {
	return Config{Version: 3, PoolMode: "source", SpeedPriority: true, UnifiedDelay: true, AutoRecover: true, Name: "我的固定出口", Nodes: []Node{}, AutoRefresh: true, RefreshMinutes: 360, ResumeOnLaunch: true, Targets: []string{"https://www.gstatic.com/generate_204", "https://cp.cloudflare.com/generate_204"}, IntervalSeconds: 45, TimeoutMS: 6000, Concurrency: 3, MinGainMS: 60, MinGainPercent: 15, MinDwellSeconds: 120, UIPort: 28180, ProxyPort: 27892, VerifyPort: 27893, ControllerPort: 28191}
}

func cloneMap(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var r map[string]any
	_ = json.Unmarshal(b, &r)
	return r
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func atomicWrite(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_ = f.Chmod(0600)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func saveConfig(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, b)
}

func loadConfig(path string) (Config, error) {
	c := defaults()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, errors.New("配置文件格式无效")
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	ports := map[int]bool{}
	for _, p := range []int{c.UIPort, c.ProxyPort, c.VerifyPort, c.ControllerPort} {
		if p < 1 || p > 65535 || ports[p] {
			return errors.New("四个本地端口必须互不相同，范围 1–65535")
		}
		ports[p] = true
	}
	if c.TimeoutMS < 1000 || c.TimeoutMS > 15000 || c.Concurrency < 1 || c.Concurrency > 6 || c.IntervalSeconds < 10 || c.IntervalSeconds > 86400 {
		return errors.New("检查间隔 10–86400 秒，超时 1000–15000 毫秒，并发 1–6")
	}
	if c.MinGainMS < 10 || c.MinGainMS > 2000 || c.MinGainPercent < 5 || c.MinGainPercent > 90 || c.MinDwellSeconds < 15 || c.MinDwellSeconds > 3600 {
		return errors.New("切换收益或驻留时间超出范围")
	}
	if len(c.Targets) < 1 || len(c.Targets) > 3 {
		return errors.New("请设置 1–3 个 HTTPS 测试目标")
	}
	for _, t := range c.Targets {
		u, err := url.Parse(t)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return errors.New("测试目标必须是无凭据的 HTTPS 地址")
		}
	}
	if c.ExpectedIP != "" && net.ParseIP(c.ExpectedIP) == nil {
		return errors.New("预期出口必须是有效 IP 地址")
	}
	if len(c.Nodes) > 2000 {
		return errors.New("最多支持 2000 条候选线路")
	}
	seen := map[string]bool{}
	for _, n := range c.Nodes {
		if len(n.ID) < 1 || len(n.ID) > 80 || strings.ContainsAny(n.ID, "/?#%\\ ") || seen[n.ID] {
			return errors.New("线路 ID 无效或重复")
		}
		seen[n.ID] = true
		if err := validateProxy(n.Proxy); err != nil {
			return fmt.Errorf("线路 %s：%w", n.ID, err)
		}
		if n.Proxy["dialer-proxy"] != nil {
			return errors.New("导入中转不得引用其他 dialer-proxy，请先展开为独立节点")
		}
	}
	if len(c.Nodes) > 0 && c.FixedExit != nil {
		if err := validateProxy(c.FixedExit); err != nil {
			return fmt.Errorf("固定出口：%w", err)
		}
	}
	return nil
}

func validateProxy(p map[string]any) error {
	if p == nil {
		return errors.New("缺少代理配置")
	}
	t, _ := p["type"].(string)
	s, _ := p["server"].(string)
	if t == "" || s == "" || p["port"] == nil {
		return errors.New("缺少 type、server 或 port")
	}
	port := integer(p["port"])
	if port < 1 || port > 65535 {
		return errors.New("服务器端口范围为 1–65535")
	}
	if n, ok := p["port"].(float64); ok && n != float64(port) {
		return errors.New("服务器端口必须是整数")
	}
	return nil
}

func buildCoreConfig(c Config, secret string) map[string]any {
	proxies := []any{}
	names := []string{"REJECT"}
	for _, n := range c.Nodes {
		p := cloneMap(n.Proxy)
		p["name"] = "relay-" + n.ID
		proxies = append(proxies, p)
		e := cloneMap(c.FixedExit)
		if c.ProbeOnly {
			e = cloneMap(n.Proxy)
		}
		e["name"] = "chain-" + n.ID
		if !c.ProbeOnly {
			e["dialer-proxy"] = "relay-" + n.ID
		}
		proxies = append(proxies, e)
		names = append(names, "chain-"+n.ID)
	}
	result := map[string]any{
		"log-level": "warning", "ipv6": true, "mode": "rule", "allow-lan": false, "bind-address": "127.0.0.1",
		"external-controller": fmt.Sprintf("127.0.0.1:%d", c.ControllerPort), "secret": secret,
		"unified-delay": c.UnifiedDelay, "tcp-concurrent": true, "profile": map[string]any{"store-selected": false, "store-fake-ip": false},
		"proxies": proxies, "proxy-groups": []any{map[string]any{"name": "ROUTE", "type": "select", "proxies": names, "default-selected": "REJECT"}, map[string]any{"name": "VERIFY", "type": "select", "proxies": names, "default-selected": "REJECT"}},
		"listeners": []any{map[string]any{"name": "applications", "type": "mixed", "listen": "127.0.0.1", "port": c.ProxyPort, "proxy": "ROUTE", "udp": false}, map[string]any{"name": "identity-check", "type": "mixed", "listen": "127.0.0.1", "port": c.VerifyPort, "proxy": "VERIFY", "udp": false}},
		"rules":     []string{"MATCH,REJECT"},
	}
	if c.ProbeOnly {
		delete(result, "listeners")
	}
	return result
}
