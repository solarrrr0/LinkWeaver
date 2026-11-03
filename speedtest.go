package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

const speedTestBytes = 2 * 1024 * 1024
const speedTestURL = "https://speed.cloudflare.com/__down?bytes=2097152"

type speedResult struct {
	Name  string  `json:"name"`
	Mbps  float64 `json:"mbps,omitempty"`
	Error string  `json:"error,omitempty"`
}

// Compare a few recent candidates through VERIFY without changing ROUTE.
func (e *Engine) speedCompare(ctx context.Context) ([]speedResult, error) {
	if !e.verifyMu.TryLock() {
		return nil, errors.New("线路验证正在进行，请稍后重试")
	}
	defer e.verifyMu.Unlock()
	e.mu.Lock()
	if e.state != "running" || e.current == "" || e.observedIP == "" {
		e.mu.Unlock()
		return nil, errors.New("请先启动并验证出口")
	}
	c, current, expected, runCtx := e.config, e.current, e.observedIP, e.runContext
	c.SpeedPriority = true
	routes := ranked(e.routes, time.Now(), c)
	ids := []string{current}
	for _, route := range routes {
		if route.ID != current && len(ids) < 3 {
			ids = append(ids, route.ID)
		}
	}
	names := make(map[string]string, len(ids))
	for _, id := range ids {
		if r := e.routes[id]; r != nil {
			names[id] = r.Label
		}
	}
	e.mu.Unlock()
	if runCtx == nil {
		return nil, errors.New("核心正在停止")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(runCtx, cancel)
	defer stop()
	defer func() {
		reset, done := context.WithTimeout(context.Background(), 2*time.Second)
		defer done()
		_ = e.selectGroup(reset, "VERIFY", "")
	}()
	results := make([]speedResult, 0, len(ids))
	for _, id := range ids {
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
		result := speedResult{Name: names[id]}
		func() {
			trial, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := e.selectGroup(trial, "VERIFY", id); err != nil {
				result.Error = "无法设置验证线路"
				return
			}
			ip, err := e.verifyIdentity(trial, c.VerifyPort)
			if err != nil || net.ParseIP(ip) == nil || !net.ParseIP(ip).Equal(net.ParseIP(expected)) {
				result.Error = "出口身份校验未通过"
				return
			}
			client := proxyClient(c.VerifyPort, 20000)
			defer client.CloseIdleConnections()
			req, _ := http.NewRequestWithContext(trial, http.MethodGet, speedTestURL, nil)
			req.Header.Set("Accept-Encoding", "identity")
			res, err := client.Do(req)
			if err != nil {
				result.Error = "下载请求失败或超时"
				return
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				result.Error = "测速服务响应异常"
				return
			}
			start := time.Now()
			n, err := io.Copy(io.Discard, io.LimitReader(res.Body, speedTestBytes))
			seconds := time.Since(start).Seconds()
			if err != nil || n != speedTestBytes || seconds <= 0 {
				result.Error = "下载未完成，无法比较"
				return
			}
			result.Mbps = float64(n) * 8 / seconds / 1e6
		}()
		results = append(results, result)
	}
	return results, nil
}

func (s *Server) speedTest(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) {
		return
	}
	e, err := s.targetEngine(r)
	if err == nil {
		var results []speedResult
		results, err = e.speedCompare(r.Context())
		if err == nil {
			respond(w, map[string]any{"results": results, "bytesPerRoute": speedTestBytes})
			return
		}
	}
	fail(w, err)
}
