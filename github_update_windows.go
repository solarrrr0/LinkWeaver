package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
)

const githubRepo = "solarrrr0/LinkWeaver"
const githubProjectURL = "https://github.com/" + githubRepo

type githubAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}
type githubRelease struct {
	Tag        string        `json:"tag_name"`
	Draft      bool          `json:"draft"`
	Prerelease bool          `json:"prerelease"`
	Assets     []githubAsset `json:"assets"`
}
type onlineUpdateState struct {
	Checking    bool        `json:"checking"`
	Downloading bool        `json:"downloading"`
	Restarting  bool        `json:"restarting"`
	Available   bool        `json:"available"`
	Latest      string      `json:"latestVersion,omitempty"`
	Message     string      `json:"message"`
	Progress    int         `json:"progress"`
	CheckedAt   time.Time   `json:"checkedAt,omitempty"`
	Asset       githubAsset `json:"-"`
	Checksum    githubAsset `json:"-"`
}

func versionNewer(candidate, current string) bool {
	if !releaseVersion.MatchString(candidate) || !releaseVersion.MatchString(current) {
		return false
	}
	a, b := strings.Split(candidate, "."), strings.Split(current, ".")
	for i := range a {
		x, e1 := strconv.ParseUint(a[i], 10, 32)
		y, e2 := strconv.ParseUint(b[i], 10, 32)
		if e1 != nil || e2 != nil {
			return false
		}
		if x != y {
			return x > y
		}
	}
	return false
}
func releaseDownloadURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && strings.HasPrefix(u.Path, "/"+githubRepo+"/releases/download/")
}
func selectRelease(r githubRelease) (onlineUpdateState, error) {
	v := strings.TrimPrefix(r.Tag, "v")
	if r.Draft || r.Prerelease || !releaseVersion.MatchString(v) {
		return onlineUpdateState{}, errors.New("GitHub 发布版本无效")
	}
	s := onlineUpdateState{Latest: v, Available: versionNewer(v, appVersion), CheckedAt: time.Now()}
	name := "LinkWeaver-Update-" + v + "-Windows-x64.zip"
	for _, a := range r.Assets {
		if a.Name == name {
			s.Asset = a
		}
		if a.Name == name+".sha256" {
			s.Checksum = a
		}
	}
	if !releaseDownloadURL(s.Asset.URL) || !releaseDownloadURL(s.Checksum.URL) || s.Asset.Size < 1 || s.Asset.Size > 200*1024*1024 {
		return onlineUpdateState{}, errors.New("此版本尚未提供完整的 Windows x64 升级包")
	}
	if s.Available {
		s.Message = "发现新版本 " + v + "，可下载并安装。"
	} else {
		s.Message = "已是最新版本。"
	}
	return s, nil
}

func (w *Workspace) githubProxies() []string {
	proxies := []string{}
	if k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE); err == nil {
		enabled, _, _ := k.GetIntegerValue("ProxyEnable")
		server, _, _ := k.GetStringValue("ProxyServer")
		k.Close()
		if enabled != 0 {
			if strings.Contains(server, "=") {
				for _, part := range strings.Split(server, ";") {
					if strings.HasPrefix(part, "http=") {
						server = strings.TrimPrefix(part, "http=")
						break
					}
				}
			}
			u, e := url.Parse("http://" + server)
			if e == nil && net.ParseIP(u.Hostname()).IsLoopback() && u.Port() != "" {
				proxies = append(proxies, u.String())
			}
		}
	}
	for _, p := range w.snapshot().Profiles {
		if e, err := w.engine(p.ID); err == nil {
			e.mu.Lock()
			ready, port := e.state == "running" && e.current != "", e.config.ProxyPort
			e.mu.Unlock()
			if ready {
				proxies = append(proxies, fmt.Sprintf("http://127.0.0.1:%d", port))
			}
		}
	}
	return append(proxies, "")
}

// All metadata and bytes are fetched from GitHub over TLS. Never ship a GitHub token.
func (w *Workspace) githubFetch(ctx context.Context, raw string, max int64, dest io.WriteSeeker, progress func(int64)) error {
	for _, proxy := range w.githubProxies() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := dest.Seek(0, io.SeekStart); err != nil {
			return err
		}
		tr := &http.Transport{TLSHandshakeTimeout: 8 * time.Second, ResponseHeaderTimeout: 12 * time.Second, DialContext: (&net.Dialer{Timeout: 8 * time.Second}).DialContext}
		if proxy != "" {
			u, _ := url.Parse(proxy)
			tr.Proxy = http.ProxyURL(u)
		}
		client := &http.Client{Transport: tr, Timeout: 3 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
			host := r.URL.Hostname()
			if len(via) > 5 || r.URL.Scheme != "https" || !(host == "github.com" || host == "api.github.com" || strings.HasSuffix(host, ".githubusercontent.com")) {
				return errors.New("不可信的升级下载重定向")
			}
			return nil
		}}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if err != nil {
			return errors.New("GitHub 更新地址无效")
		}
		req.Header.Set("User-Agent", "LinkWeaver/"+appVersion)
		req.Header.Set("Accept", "application/vnd.github+json")
		res, err := client.Do(req)
		if err != nil {
			tr.CloseIdleConnections()
			continue
		}
		if res.StatusCode != http.StatusOK {
			res.Body.Close()
			tr.CloseIdleConnections()
			if res.StatusCode == 403 || res.StatusCode == 429 {
				return errors.New("GitHub 暂时限制更新检查，请稍后重试")
			}
			if res.StatusCode == 404 {
				return errors.New("GitHub 尚无可用的正式发布版本")
			}
			continue
		}
		reader := io.LimitReader(res.Body, max+1)
		var written int64
		buf := make([]byte, 64*1024)
		for {
			n, e := reader.Read(buf)
			if n > 0 {
				var size int
				size, err = dest.Write(buf[:n])
				written += int64(size)
				if err != nil {
					break
				}
				if size != n {
					err = io.ErrShortWrite
					break
				}
				if progress != nil {
					progress(written)
				}
			}
			if e != nil {
				if e != io.EOF {
					err = e
				}
				break
			}
		}
		res.Body.Close()
		tr.CloseIdleConnections()
		if written > max {
			return errors.New("GitHub 更新文件过大")
		}
		if err == nil {
			if f, ok := dest.(*os.File); ok {
				return f.Truncate(written)
			}
			return nil
		}
	}
	return errors.New("连接 GitHub 失败，请检查网络或已连接的代理")
}
func (w *Workspace) fetchRelease(ctx context.Context) (onlineUpdateState, error) {
	if err := os.MkdirAll(filepath.Join(w.root, "runtime"), 0700); err != nil {
		return onlineUpdateState{}, err
	}
	f, err := os.CreateTemp(filepath.Join(w.root, "runtime"), "github-release-*.json")
	if err != nil {
		return onlineUpdateState{}, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = w.githubFetch(ctx, "https://api.github.com/repos/"+githubRepo+"/releases/latest", 1024*1024, f, nil); err != nil {
		return onlineUpdateState{}, err
	}
	f.Seek(0, io.SeekStart)
	var r githubRelease
	if json.NewDecoder(f).Decode(&r) != nil {
		return onlineUpdateState{}, errors.New("GitHub 发布信息无法解析")
	}
	return selectRelease(r)
}
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	s.updateMu.Lock()
	if s.online.Checking || s.online.Downloading || s.online.Restarting {
		s.updateMu.Unlock()
		respond(w, map[string]bool{"ok": true})
		return
	}
	s.online.Checking = true
	s.online.Message = "正在检查 GitHub 正式版本…"
	s.updateMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		state, err := s.workspace.fetchRelease(ctx)
		s.updateMu.Lock()
		defer s.updateMu.Unlock()
		if err != nil {
			s.online.Checking = false
			s.online.Message = err.Error()
			return
		}
		s.online = state
	}()
	respond(w, map[string]bool{"ok": true})
}
func (s *Server) updateOnline(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	s.updateMu.Lock()
	state := s.online
	if state.Checking || state.Downloading || state.Restarting || !state.Available {
		s.updateMu.Unlock()
		fail(w, errors.New("请先检查更新并等待可用版本"))
		return
	}
	s.online.Downloading = true
	s.online.Progress = 0
	s.online.Message = "正在下载更新…"
	s.updateMu.Unlock()
	id, err := s.workspace.startJob("program-update", "下载程序更新", func(ctx context.Context, j *Job) (result error) {
		defer func() {
			if result != nil {
				s.updateMu.Lock()
				s.online.Downloading = false
				s.online.Message = result.Error()
				s.updateMu.Unlock()
			}
		}()
		dir, err := os.MkdirTemp(filepath.Join(s.workspace.root, "runtime"), "github-download-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		checksum, err := os.Create(filepath.Join(dir, "checksum"))
		if err != nil {
			return err
		}
		err = s.workspace.githubFetch(ctx, state.Checksum.URL, 1024, checksum, nil)
		checksum.Close()
		if err != nil {
			return err
		}
		b, _ := os.ReadFile(filepath.Join(dir, "checksum"))
		fields := strings.Fields(string(b))
		if len(fields) < 1 {
			return errors.New("升级校验文件无效")
		}
		expected := strings.ToLower(fields[0])
		hash, err := hex.DecodeString(expected)
		if err != nil || len(hash) != 32 {
			return errors.New("升级 SHA256 校验值无效")
		}
		if state.Asset.Digest != "" && state.Asset.Digest != "sha256:"+expected {
			return errors.New("GitHub 文件摘要与校验文件不一致")
		}
		path := filepath.Join(dir, "update.zip")
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		err = s.workspace.githubFetch(ctx, state.Asset.URL, 200*1024*1024, f, func(n int64) {
			percent := int(min(int64(99), n*100/state.Asset.Size))
			s.updateMu.Lock()
			s.online.Progress = percent
			s.updateMu.Unlock()
			s.workspace.progress(j, percent, 100, 0, "正在下载 GitHub 更新")
		})
		f.Close()
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.workspace.op.Lock()
		defer s.workspace.op.Unlock()
		plan, err := s.workspace.prepareUpdate(path, expected)
		if err != nil {
			return err
		}
		if plan.Version != state.Latest || !versionNewer(plan.Version, appVersion) {
			return errors.New("升级包版本与 GitHub 发布版本不一致")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = s.workspace.launchUpdate(plan); err != nil {
			return err
		}
		s.updateMu.Lock()
		s.online.Progress = 100
		s.online.Downloading = false
		s.online.Restarting = true
		s.online.Message = "校验通过，正在更新并重启…"
		s.updateMu.Unlock()
		s.workspace.progress(j, 100, 100, 0, "校验通过，正在更新并重启")
		select {
		case s.shutdown <- struct{}{}:
		default:
		}
		return nil
	})
	if err != nil {
		s.updateMu.Lock()
		s.online.Downloading = false
		s.updateMu.Unlock()
		fail(w, err)
		return
	}
	respond(w, map[string]any{"ok": true, "jobID": id})
}
