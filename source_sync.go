package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"
)

func (w *Workspace) fetchSubscription(s RelaySource) ([]map[string]any, error) {
	return w.fetchSubscriptionContext(context.Background(), s)
}
func (w *Workspace) fetchSubscriptionContext(ctx context.Context, s RelaySource) ([]map[string]any, error) {
	transport := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 8 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	client.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return errors.New("重定向次数过多")
		}
		return nil
	}
	request := func() ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", s.URL, nil)
		if err != nil {
			return nil, errors.New("订阅地址无效")
		}
		agent := s.UserAgent
		if agent == "" {
			agent = "Clash.Meta"
		}
		req.Header.Set("User-Agent", agent)
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("订阅请求失败，请检查网络和地址")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("订阅返回 HTTP %d", resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024+1))
		if err != nil || len(b) > 4*1024*1024 {
			return nil, errors.New("订阅读取失败或超过 4 MB")
		}
		return b, nil
	}
	b, err := request()
	if err != nil && ctx.Err() == nil {
		c := w.snapshot()
		for _, p := range c.Profiles {
			e, _ := w.engine(p.ID)
			if e == nil {
				continue
			}
			e.mu.Lock()
			ready := e.current != "" && e.state == "running"
			port := e.config.ProxyPort
			e.mu.Unlock()
			if ready {
				u, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
				transport.Proxy = http.ProxyURL(u)
				b, err = request()
				break
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return parseProxyText(string(b))
}
func (w *Workspace) refreshSource(id string) error {
	return w.refreshSourceContext(context.Background(), id)
}
func (w *Workspace) refreshSourceContext(ctx context.Context, id string) error {
	before := w.snapshot()
	var source RelaySource
	found := false
	for _, s := range before.Sources {
		if s.ID == id {
			source = s
			found = true
		}
	}
	if !found {
		return errors.New("来源不存在")
	}
	if !source.Enabled {
		return errors.New("请先启用此来源")
	}
	if source.URL == "" {
		return errors.New("此来源是本地节点，没有订阅地址")
	}
	proxies, fetchErr := w.fetchSubscriptionContext(ctx, source)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	w.op.Lock()
	defer w.op.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c := w.snapshot()
	idx := -1
	for i, s := range c.Sources {
		if s.ID == id {
			idx = i
		}
	}
	if idx < 0 || !c.Sources[idx].Enabled || c.Sources[idx].URL != source.URL || c.Sources[idx].UserAgent != source.UserAgent {
		return errors.New("获取期间来源已修改，本次结果未应用")
	}
	if fetchErr != nil {
		c.Sources[idx].LastError = fetchErr.Error()
		_ = w.commit(c)
		return fetchErr
	}
	c = mergeSourceNodes(c, c.Sources[idx], proxies)
	c.Sources[idx].UpdatedAt = time.Now()
	c.Sources[idx].LastError = ""
	return w.commit(c)
}
func mergeSourceNodes(c Config, s RelaySource, proxies []map[string]any) Config {
	positions := map[string]int{}
	for i := range c.Nodes {
		n := &c.Nodes[i]
		positions[proxyID(n.Proxy)] = i
		if slices.Contains(nodeSources(*n), s.ID) {
			ids := []string{}
			for _, id := range nodeSources(*n) {
				if id != s.ID {
					ids = append(ids, id)
				}
			}
			setNodeSources(n, ids)
			if len(ids) == 0 {
				n.Retired = true
			}
		}
	}
	for _, raw := range proxies {
		key := proxyID(raw)
		if slices.Contains(c.DeletedNodes, key) {
			continue
		}
		p := cloneMap(raw)
		name, _ := p["name"].(string)
		if name == "" {
			name = "订阅线路"
		}
		delete(p, "name")
		if index, ok := positions[key]; ok {
			n := &c.Nodes[index]
			ids := nodeSources(*n)
			if !slices.Contains(ids, s.ID) {
				ids = append(ids, s.ID)
			}
			setNodeSources(n, ids)
			n.Retired = false
			continue
		}
		c.Nodes = append(c.Nodes, Node{ID: key, Label: name, Proxy: p, SourceID: s.ID, SourceIDs: []string{s.ID}, Priority: s.Priority})
		positions[key] = len(c.Nodes) - 1
	}
	// Keep removed nodes visible as retired, but cap growth on repeated subscription churn.
	if len(c.Nodes) > 2000 {
		active := []Node{}
		retired := []Node{}
		for _, n := range c.Nodes {
			if n.Retired {
				retired = append(retired, n)
			} else {
				active = append(active, n)
			}
		}
		if len(active) < 2000 {
			active = append(active, retired[max(0, len(retired)-(2000-len(active))):]...)
		}
		c.Nodes = active
	}
	return c
}
