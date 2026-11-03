package main

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

type ClashEntrance struct {
	Name   string `yaml:"name"`
	Type   string `yaml:"type"`
	Server string `yaml:"server"`
	Port   int    `yaml:"port"`
	UDP    bool   `yaml:"udp"`
}
type clashGroup struct {
	Name    string   `yaml:"name"`
	Type    string   `yaml:"type"`
	Proxies []string `yaml:"proxies"`
}
type clashDocument struct {
	Mode     string          `yaml:"mode"`
	AllowLAN bool            `yaml:"allow-lan"`
	Proxies  []ClashEntrance `yaml:"proxies"`
	Groups   []clashGroup    `yaml:"proxy-groups"`
	Rules    []string        `yaml:"rules"`
}

func (s *Server) subscriptionKey() (string, error) {
	s.clashKeyOnce.Do(func() {
		if s.workspace == nil {
			s.clashKeyErr = errors.New("工作区尚未初始化")
			return
		}
		path := filepath.Join(s.workspace.root, "data", "clash-subscription.key")
		b, err := os.ReadFile(path)
		if err == nil {
			value := strings.TrimSpace(string(b))
			raw, decodeErr := hex.DecodeString(value)
			if decodeErr != nil || len(raw) != 32 {
				s.clashKeyErr = errors.New("Clash 订阅密钥文件无效，请退出后恢复备份")
				return
			}
			s.clashKey = value
			return
		}
		if !os.IsNotExist(err) {
			s.clashKeyErr = errors.New("无法读取 Clash 订阅密钥")
			return
		}
		value := randomToken()
		if err = atomicWrite(path, []byte(value)); err != nil {
			s.clashKeyErr = errors.New("无法保存 Clash 订阅密钥")
			return
		}
		s.clashKey = value
	})
	return s.clashKey, s.clashKeyErr
}

// Only local, ready listeners are exported. Upstream endpoints and credentials
// stay in LinkWeaver; Clash forwards new connections to its stable SOCKS port.
func (s *Server) clashEntrances(profile string) ([]ClashEntrance, error) {
	if s.workspace == nil {
		return nil, errors.New("工作区尚未初始化")
	}
	c := s.workspace.snapshot()
	found := profile == ""
	entries := []ClashEntrance{}
	for _, p := range c.Profiles {
		if profile != "" && p.ID != profile {
			continue
		}
		found = true
		e, err := s.workspace.engine(p.ID)
		if err != nil {
			continue
		}
		e.mu.Lock()
		ready := e.state == "running" && e.current != "" && e.observedIP != ""
		port := e.config.ProxyPort
		e.mu.Unlock()
		if ready {
			entries = append(entries, ClashEntrance{Name: fmt.Sprintf("链跃 · %s [%d]", p.Name, port), Type: "socks5", Server: "127.0.0.1", Port: port, UDP: false})
		}
	}
	if !found {
		return nil, errors.New("出口不存在")
	}
	if len(entries) == 0 {
		return nil, errors.New("没有已连接并通过出口校验的出口，请先启动出口并等待连接成功")
	}
	return entries, nil
}

func encodeClashSubscription(entries []ClashEntrance) ([]byte, error) {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	doc := clashDocument{Mode: "rule", AllowLAN: false, Proxies: entries, Groups: []clashGroup{{Name: "链跃出口", Type: "select", Proxies: names}}, Rules: []string{"MATCH,链跃出口"}}
	return yaml.Marshal(doc)
}

func (s *Server) clashLinks(profile string) (string, string, int, error) {
	entries, err := s.clashEntrances(profile)
	if err != nil {
		return "", "", 0, err
	}
	key, err := s.subscriptionKey()
	if err != nil {
		return "", "", 0, err
	}
	q := url.Values{"key": {key}}
	if profile != "" {
		q.Set("profile", profile)
	}
	link := fmt.Sprintf("http://127.0.0.1:%d/clash/subscription.yaml?%s", s.workspace.snapshot().UIPort, q.Encode())
	name := "链跃 LinkWeaver"
	if len(entries) == 1 {
		name = entries[0].Name
	}
	uri := "clash://install-config?" + url.Values{"url": {link}, "name": {name}}.Encode()
	return link, uri, len(entries), nil
}

func (s *Server) clashExportInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	link, uri, count, err := s.clashLinks(r.URL.Query().Get("profile"))
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]any{"subscriptionURL": link, "installURI": uri, "entranceCount": count, "localOnly": true})
}

func (s *Server) clashInstall(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) {
		return
	}
	var input struct {
		Profile string `json:"profile"`
	}
	if err := decode(w, r, &input); err != nil {
		fail(w, err)
		return
	}
	_, uri, count, err := s.clashLinks(input.Profile)
	if err != nil {
		fail(w, err)
		return
	}
	open := s.openClashURI
	if open == nil {
		open = launchClashURI
	}
	if err = open(uri); err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]any{"requested": true, "entranceCount": count})
}

func (s *Server) clashSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(405)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
		http.Error(w, "Forbidden", 403)
		return
	}
	key, err := s.subscriptionKey()
	if err != nil {
		http.Error(w, "Subscription unavailable", 503)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("key")), []byte(key)) != 1 {
		http.Error(w, "Forbidden", 403)
		return
	}
	entries, err := s.clashEntrances(r.URL.Query().Get("profile"))
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(409)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(err.Error()))
		}
		return
	}
	data, err := encodeClashSubscription(entries)
	if err != nil {
		http.Error(w, "Subscription unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="LinkWeaver.yaml"`)
	w.Header().Set("profile-update-interval", "6")
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
