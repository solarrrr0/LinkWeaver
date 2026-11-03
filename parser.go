package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

func decodeBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("Base64 编码无效")
}
func integer(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	default:
		return 0
	}
}

func parseProxyText(text string) ([]map[string]any, error) {
	return parseProxyDepth(strings.TrimSpace(strings.TrimPrefix(text, "\ufeff")), 0)
}
func parseProxyDepth(text string, depth int) ([]map[string]any, error) {
	if len(text) == 0 {
		return nil, errors.New("请粘贴节点链接或 Clash YAML/JSON")
	}
	if len(text) > 4*1024*1024 || depth > 1 {
		return nil, errors.New("导入内容过大或嵌套过深")
	}
	var doc any
	if yaml.Unmarshal([]byte(text), &doc) == nil {
		switch v := doc.(type) {
		case map[string]any:
			if raw, ok := v["proxies"].([]any); ok {
				return mapList(raw)
			}
			if v["type"] != nil {
				return cleanProxyList([]map[string]any{v})
			}
		case []any:
			return mapList(v)
		}
	}
	lines := strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' })
	out := []map[string]any{}
	if strings.Contains(text, "://") {
		for i, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			p, err := parseURI(line)
			if err != nil {
				return nil, fmt.Errorf("第 %d 行：%w", i+1, err)
			}
			out = append(out, p)
		}
		return cleanProxyList(out)
	}
	if b, err := decodeBase64(strings.Join(strings.Fields(text), "")); err == nil && len(b) > 0 {
		return parseProxyDepth(string(b), depth+1)
	}
	return nil, errors.New("无法识别内容，支持节点链接、Base64 订阅和 Clash YAML/JSON")
}
func mapList(raw []any) ([]map[string]any, error) {
	out := []map[string]any{}
	for _, x := range raw {
		m, ok := x.(map[string]any)
		if !ok {
			return nil, errors.New("节点列表格式无效")
		}
		out = append(out, m)
	}
	return cleanProxyList(out)
}
func cleanProxyList(in []map[string]any) ([]map[string]any, error) {
	if len(in) == 0 {
		return nil, errors.New("没有发现节点")
	}
	if len(in) > 2000 {
		return nil, errors.New("最多支持 2000 个节点")
	}
	out := []map[string]any{}
	for i, p := range in {
		if err := validateProxy(p); err != nil {
			return nil, fmt.Errorf("第 %d 个节点缺少协议、服务器或端口", i+1)
		}
		if integer(p["port"]) < 1 || integer(p["port"]) > 65535 {
			return nil, fmt.Errorf("第 %d 个节点端口无效", i+1)
		}
		if p["dialer-proxy"] != nil {
			return nil, errors.New("请导入独立中转节点，不要导入引用其他代理的链路")
		}
		q := cloneMap(p)
		q["port"] = integer(p["port"])
		out = append(out, q)
	}
	return out, nil
}

func parseURI(raw string) (map[string]any, error) {
	if strings.HasPrefix(strings.ToLower(raw), "vmess://") {
		b, err := decodeBase64(raw[len("vmess://"):])
		if err != nil {
			return nil, errors.New("VMess 链接编码无效")
		}
		var v map[string]any
		if json.Unmarshal(b, &v) != nil {
			return nil, errors.New("VMess 内容格式无效")
		}
		p := map[string]any{"name": v["ps"], "type": "vmess", "server": v["add"], "port": integer(v["port"]), "uuid": v["id"], "alterId": integer(v["aid"]), "cipher": "auto"}
		if v["scy"] != nil {
			p["cipher"] = v["scy"]
		}
		if netw, _ := v["net"].(string); netw != "" && netw != "tcp" {
			p["network"] = netw
		}
		if v["tls"] == "tls" {
			p["tls"] = true
		}
		if v["sni"] != "" && v["sni"] != nil {
			p["servername"] = v["sni"]
		}
		if v["net"] == "ws" {
			opts := map[string]any{"path": v["path"]}
			if v["host"] != "" && v["host"] != nil {
				opts["headers"] = map[string]any{"Host": v["host"]}
			}
			p["ws-opts"] = opts
		}
		if v["net"] == "grpc" {
			p["grpc-opts"] = map[string]any{"grpc-service-name": v["path"]}
		}
		return p, nil
	}
	if strings.HasPrefix(raw, "ss://") {
		return parseSS(raw)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil, errors.New("节点链接格式无效")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("请在节点链接中填写有效端口")
	}
	typ := strings.ToLower(u.Scheme)
	if typ == "hy2" {
		typ = "hysteria2"
	}
	if typ == "socks" {
		typ = "socks5"
	}
	p := map[string]any{"name": u.Fragment, "type": typ, "server": u.Hostname(), "port": port}
	user, pass := "", ""
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	q := u.Query()
	switch typ {
	case "socks5", "http", "https":
		if typ == "https" {
			p["type"] = "http"
			p["tls"] = true
		}
		if user != "" {
			p["username"] = user
		}
		if pass != "" {
			p["password"] = pass
		}
	case "vless":
		p["uuid"] = user
		if q.Get("flow") != "" {
			p["flow"] = q.Get("flow")
		}
		if q.Get("security") == "tls" || q.Get("security") == "reality" {
			p["tls"] = true
		}
		if q.Get("security") == "reality" {
			opts := map[string]any{"public-key": q.Get("pbk")}
			if q.Get("sid") != "" {
				opts["short-id"] = q.Get("sid")
			}
			p["reality-opts"] = opts
		}
	case "trojan", "anytls":
		p["password"] = user
	case "hysteria2":
		p["password"] = user
		if pass != "" {
			p["password"] = user + ":" + pass
		}
		if q.Get("obfs") != "" {
			p["obfs"] = q.Get("obfs")
		}
		if q.Get("obfs-password") != "" {
			p["obfs-password"] = q.Get("obfs-password")
		}
		if q.Get("insecure") == "1" || q.Get("insecure") == "true" {
			p["skip-cert-verify"] = true
		}
	case "tuic":
		p["uuid"] = user
		p["password"] = pass
		if q.Get("congestion_control") != "" {
			p["congestion-controller"] = q.Get("congestion_control")
		}
		if q.Get("udp_relay_mode") != "" {
			p["udp-relay-mode"] = q.Get("udp_relay_mode")
		}
		if q.Get("alpn") != "" {
			p["alpn"] = strings.Split(q.Get("alpn"), ",")
		}
	default:
		return nil, errors.New("此链接协议暂未支持；可导入原始 Clash YAML 保留参数")
	}
	if q.Get("allowInsecure") == "1" {
		p["skip-cert-verify"] = true
	}
	if q.Get("sni") != "" {
		key := "sni"
		if typ == "vless" {
			key = "servername"
		}
		p[key] = q.Get("sni")
	}
	if q.Get("fp") != "" {
		p["client-fingerprint"] = q.Get("fp")
	}
	if q.Get("type") != "" && q.Get("type") != "tcp" {
		p["network"] = q.Get("type")
	}
	if q.Get("type") == "ws" {
		opts := map[string]any{"path": q.Get("path")}
		if q.Get("host") != "" {
			opts["headers"] = map[string]any{"Host": q.Get("host")}
		}
		p["ws-opts"] = opts
	}
	if q.Get("type") == "grpc" {
		p["grpc-opts"] = map[string]any{"grpc-service-name": q.Get("serviceName")}
	}
	return p, nil
}
func parseSS(raw string) (map[string]any, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("SS 链接格式无效")
	}
	body := strings.TrimPrefix(raw, "ss://")
	body = strings.SplitN(body, "#", 2)[0]
	body = strings.SplitN(body, "?", 2)[0]
	var userpass, host string
	if at := strings.LastIndex(body, "@"); at >= 0 {
		userpass = body[:at]
		host = body[at+1:]
		if b, e := decodeBase64(userpass); e == nil {
			userpass = string(b)
		} else {
			userpass, _ = url.PathUnescape(userpass)
		}
	} else {
		b, e := decodeBase64(body)
		if e != nil {
			return nil, errors.New("SS 链接编码无效")
		}
		s := string(b)
		at := strings.LastIndex(s, "@")
		if at < 0 {
			return nil, errors.New("SS 链接缺少服务器")
		}
		userpass = s[:at]
		host = s[at+1:]
	}
	parts := strings.SplitN(userpass, ":", 2)
	server, portStr, e := net.SplitHostPort(host)
	port, _ := strconv.Atoi(portStr)
	if e != nil || len(parts) != 2 || port < 1 || port > 65535 {
		return nil, errors.New("SS 链接字段无效")
	}
	p := map[string]any{"name": u.Fragment, "type": "ss", "server": server, "port": port, "cipher": parts[0], "password": parts[1]}
	if u.Query().Get("plugin") != "" {
		return nil, errors.New("带插件的 SS 链接请使用 Clash YAML 导入，以保留完整插件参数")
	}
	return p, nil
}
