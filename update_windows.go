package main

import (
	"archive/zip"
	"crypto/sha256"
	"debug/pe"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const appVersion = "0.3.3"

type releaseManifest struct {
	Product string            `json:"product"`
	Version string            `json:"version"`
	Files   map[string]string `json:"files"`
}
type updatePlan struct {
	Root    string            `json:"root"`
	Stage   string            `json:"stage"`
	Version string            `json:"version"`
	Parent  int               `json:"parent"`
	Port    int               `json:"port"`
	Files   map[string]string `json:"files"`
}
type updateResult struct {
	State   string    `json:"state"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
	Backup  string    `json:"backup,omitempty"`
}

var releaseVersion = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func allowedUpdateFile(name string) bool { return name == "LinkWeaver.exe" || name == "bin/mihomo.exe" }
func validateUpdatePE(path string) error {
	f, err := pe.Open(path)
	if err != nil {
		return errors.New("升级文件不是有效的 Windows 程序")
	}
	defer f.Close()
	if f.Machine != pe.IMAGE_FILE_MACHINE_AMD64 {
		return errors.New("升级文件必须是 Windows x64 程序")
	}
	return nil
}
func (w *Workspace) prepareUpdate(path, expected string) (updatePlan, error) {
	var plan updatePlan
	expected = strings.ToLower(strings.TrimSpace(expected))
	hash, err := hex.DecodeString(expected)
	if err != nil || len(hash) != 32 {
		return plan, errors.New("请填写可信发布来源提供的完整 SHA256 校验值")
	}
	actual, err := fileSHA(path)
	if err != nil {
		return plan, errors.New("无法读取升级包，请填写本机 ZIP 的完整路径")
	}
	if actual != expected {
		return plan, errors.New("升级包 SHA256 不匹配，未应用")
	}
	archive, err := zip.OpenReader(path)
	if err != nil {
		return plan, errors.New("升级包必须是 ZIP 文件")
	}
	defer archive.Close()
	var manifest releaseManifest
	prefix := ""
	found := false
	for _, f := range archive.File {
		if filepath.Base(f.Name) == "release.json" {
			if found || f.UncompressedSize64 > 16384 {
				return plan, errors.New("升级包发布清单无效")
			}
			r, e := f.Open()
			if e != nil {
				return plan, e
			}
			e = json.NewDecoder(io.LimitReader(r, 16384)).Decode(&manifest)
			r.Close()
			if e != nil {
				return plan, errors.New("升级包发布清单无法解析")
			}
			prefix = strings.TrimSuffix(f.Name, "release.json")
			found = true
		}
	}
	if !found || manifest.Product != "LinkWeaver" || !releaseVersion.MatchString(manifest.Version) || len(manifest.Files) != 2 {
		return plan, errors.New("升级包缺少链跃发布清单或版本无效")
	}
	for name, hash := range manifest.Files {
		if !allowedUpdateFile(name) || len(hash) != 64 {
			return plan, errors.New("升级包文件清单无效")
		}
	}
	stage := filepath.Join(w.root, "runtime", "updates", randomToken()[:12])
	if err = os.MkdirAll(stage, 0700); err != nil {
		return plan, err
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(stage)
		}
	}()
	copied := map[string]bool{}
	for _, f := range archive.File {
		if !strings.HasPrefix(f.Name, prefix) {
			continue
		}
		name := strings.TrimPrefix(f.Name, prefix)
		expected, ok := manifest.Files[name]
		if !ok {
			continue
		}
		if copied[name] || f.UncompressedSize64 > 200*1024*1024 {
			return plan, errors.New("升级包文件重复或过大")
		}
		r, e := f.Open()
		if e != nil {
			return plan, e
		}
		b, e := io.ReadAll(io.LimitReader(r, 200*1024*1024+1))
		r.Close()
		if e != nil || len(b) > 200*1024*1024 {
			return plan, errors.New("读取升级文件失败")
		}
		digest := sha256.Sum256(b)
		if hex.EncodeToString(digest[:]) != expected {
			return plan, errors.New("升级文件与发布清单不一致")
		}
		target := filepath.Join(stage, filepath.FromSlash(name))
		if e = atomicWrite(target, b); e != nil {
			return plan, e
		}
		if e = validateUpdatePE(target); e != nil {
			return plan, e
		}
		copied[name] = true
	}
	if len(copied) != 2 {
		return plan, errors.New("升级包文件不完整")
	}
	plan = updatePlan{Root: w.root, Stage: stage, Version: manifest.Version, Files: manifest.Files, Port: w.snapshot().UIPort}
	b, _ := json.Marshal(plan)
	if err = atomicWrite(filepath.Join(w.root, "data", "update-prepared.json"), b); err != nil {
		return plan, err
	}
	success = true
	return plan, nil
}
func (w *Workspace) preparedUpdate() (updatePlan, error) {
	var p updatePlan
	b, err := os.ReadFile(filepath.Join(w.root, "data", "update-prepared.json"))
	if err != nil || json.Unmarshal(b, &p) != nil {
		return p, errors.New("请先校验本地升级包")
	}
	if p.Root != w.root || !strings.HasPrefix(p.Stage, filepath.Join(w.root, "runtime", "updates")+string(os.PathSeparator)) || !releaseVersion.MatchString(p.Version) || len(p.Files) != 2 {
		return p, errors.New("待升级记录无效")
	}
	for name, expected := range p.Files {
		if !allowedUpdateFile(name) {
			return p, errors.New("待升级文件无效")
		}
		actual, err := fileSHA(filepath.Join(p.Stage, filepath.FromSlash(name)))
		if err != nil || actual != expected {
			return p, errors.New("待升级文件已变化，请重新校验")
		}
	}
	return p, nil
}
func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" || !s.requireWorkspace(w) {
		return
	}
	s.updateMu.Lock()
	online := s.online
	s.updateMu.Unlock()
	if online.Message == "" {
		online.Message = "打开管理页时自动检查 GitHub 更新。"
	}
	v := map[string]any{"version": appVersion, "onlineConfigured": true, "projectURL": githubProjectURL, "message": online.Message, "online": online}
	if p, err := s.workspace.preparedUpdate(); err == nil {
		v["preparedVersion"] = p.Version
	}
	var last updateResult
	if b, err := os.ReadFile(filepath.Join(s.workspace.root, "data", "update-result.json")); err == nil && json.Unmarshal(b, &last) == nil {
		v["lastResult"] = last
	}
	if _, err := os.Stat(filepath.Join(s.workspace.root, "data", "update-previous.json")); err == nil {
		v["canRollback"] = true
	}
	respond(w, v)
}
func (s *Server) updatePrepare(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	var v struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	}
	if err := decode(w, r, &v); err != nil {
		fail(w, err)
		return
	}
	s.workspace.op.Lock()
	defer s.workspace.op.Unlock()
	p, err := s.workspace.prepareUpdate(strings.Trim(strings.TrimSpace(v.Path), `"`), v.SHA256)
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]any{"ok": true, "version": p.Version})
}
func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	s.workspace.op.Lock()
	defer s.workspace.op.Unlock()
	p, err := s.workspace.preparedUpdate()
	if err == nil {
		err = s.workspace.launchUpdate(p)
	}
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]bool{"restarting": true})
	select {
	case s.shutdown <- struct{}{}:
	default:
	}
}
func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	s.workspace.op.Lock()
	defer s.workspace.op.Unlock()
	var prev updatePlan
	b, err := os.ReadFile(filepath.Join(s.workspace.root, "data", "update-previous.json"))
	if err == nil {
		err = json.Unmarshal(b, &prev)
	}
	if err != nil || prev.Root != s.workspace.root || len(prev.Files) != 2 || !strings.HasPrefix(prev.Stage, filepath.Join(prev.Root, "data", "backups", "upgrade-")) {
		fail(w, errors.New("没有可恢复的程序版本"))
		return
	}
	for name, hash := range prev.Files {
		if !allowedUpdateFile(name) {
			fail(w, errors.New("回退记录无效"))
			return
		}
		actual, e := fileSHA(filepath.Join(prev.Stage, filepath.FromSlash(name)))
		if e != nil || actual != hash {
			fail(w, errors.New("回退备份校验失败"))
			return
		}
	}
	if err = s.workspace.launchUpdate(prev); err != nil {
		fail(w, err)
		return
	}
	respond(w, map[string]bool{"restarting": true})
	select {
	case s.shutdown <- struct{}{}:
	default:
	}
}
func (w *Workspace) launchUpdate(p updatePlan) error {
	p.Parent = os.Getpid()
	p.Port = w.snapshot().UIPort
	b, _ := json.Marshal(p)
	planPath := filepath.Join(w.root, "runtime", "update-plan.json")
	if err := atomicWrite(planPath, b); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	worker := filepath.Join(w.root, "runtime", "update-worker.exe")
	if err = copyUpdateFile(exe, worker); err != nil {
		return errors.New("无法准备升级助手")
	}
	cmd := exec.Command(worker, "--apply-update", planPath)
	hideWindow(cmd)
	if err = cmd.Start(); err != nil {
		return errors.New("无法启动升级助手")
	}
	return nil
}
func copyUpdateFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return atomicWrite(dst, b)
}
func updateWorker(path string) {
	var p updatePlan
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &p) != nil || p.Parent <= 0 || len(p.Files) != 2 {
		return
	}
	if parent, e := os.FindProcess(p.Parent); e == nil {
		_, _ = parent.Wait()
	}
	result := updateResult{State: "failed", Message: "升级未完成，保留原程序", At: time.Now()}
	defer func() {
		b, _ := json.Marshal(result)
		_ = atomicWrite(filepath.Join(p.Root, "data", "update-result.json"), b)
	}()
	backup := filepath.Join(p.Root, "data", "backups", "upgrade-"+time.Now().Format("20060102-150405")+"-"+randomToken()[:6])
	previous := updatePlan{Root: p.Root, Stage: backup, Version: appVersion, Files: map[string]string{}, Port: p.Port}
	for name, hash := range p.Files {
		if !allowedUpdateFile(name) {
			return
		}
		actual, e := fileSHA(filepath.Join(p.Stage, filepath.FromSlash(name)))
		if e != nil || actual != hash {
			return
		}
		dst := filepath.Join(p.Root, filepath.FromSlash(name))
		if e = copyUpdateFile(dst, filepath.Join(backup, filepath.FromSlash(name))); e != nil {
			return
		}
		previous.Files[name], _ = fileSHA(dst)
	}
	if err = copyUpdateFile(filepath.Join(p.Root, "data", "config.json"), filepath.Join(backup, "config.json")); err != nil {
		return
	}
	restore := func() {
		for name := range previous.Files {
			_ = copyUpdateFile(filepath.Join(backup, filepath.FromSlash(name)), filepath.Join(p.Root, filepath.FromSlash(name)))
		}
		_ = copyUpdateFile(filepath.Join(backup, "config.json"), filepath.Join(p.Root, "data", "config.json"))
	}
	for name := range p.Files {
		if err = copyUpdateFile(filepath.Join(p.Stage, filepath.FromSlash(name)), filepath.Join(p.Root, filepath.FromSlash(name))); err != nil {
			restore()
			restartUpdated(p.Root)
			result.Message = "文件替换失败，已尝试恢复旧版本"
			return
		}
	}
	child := restartUpdated(p.Root)
	ready := false
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	var shutdownToken string
	for i := 0; i < 40; i++ {
		time.Sleep(250 * time.Millisecond)
		resp, e := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", p.Port))
		if e == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
			resp.Body.Close()
			match := regexp.MustCompile(`name="lw-token" content="([^"]+)"`).FindSubmatch(body)
			if len(match) == 2 {
				shutdownToken = string(match[1])
				req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/api/workspace", p.Port), nil)
				req.Header.Set("X-LinkWeaver-Token", shutdownToken)
				if view, e := client.Do(req); e == nil {
					var state struct {
						Version string `json:"version"`
						Tray    bool   `json:"trayRegistered"`
					}
					_ = json.NewDecoder(view.Body).Decode(&state)
					view.Body.Close()
					if state.Version == p.Version && state.Tray {
						ready = true
						break
					}
				}
			}
		}
	}
	if !ready {
		if shutdownToken != "" {
			req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/shutdown", p.Port), strings.NewReader("{}"))
			req.Header.Set("X-LinkWeaver-Token", shutdownToken)
			req.Header.Set("Content-Type", "application/json")
			if resp, e := client.Do(req); e == nil {
				resp.Body.Close()
				time.Sleep(time.Second)
			}
		}
		if child != nil && child.Process != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
		restore()
		restartUpdated(p.Root)
		result.Message = "新版未通过启动检查，已尝试恢复旧程序及配置"
		result.Backup = backup
		return
	}
	previousBytes, _ := json.Marshal(previous)
	var obsolete updatePlan
	if b, e := os.ReadFile(filepath.Join(p.Root, "data", "update-previous.json")); e == nil {
		_ = json.Unmarshal(b, &obsolete)
	}
	_ = atomicWrite(filepath.Join(p.Root, "data", "update-previous.json"), previousBytes)
	_ = os.Remove(filepath.Join(p.Root, "data", "update-prepared.json"))
	backupRoot := filepath.Join(p.Root, "data", "backups") + string(os.PathSeparator)
	if obsolete.Root == p.Root && obsolete.Stage != backup && strings.HasPrefix(filepath.Clean(obsolete.Stage), backupRoot+"upgrade-") {
		_ = os.RemoveAll(obsolete.Stage)
	}
	if strings.HasPrefix(filepath.Clean(p.Stage), filepath.Join(p.Root, "runtime", "updates")+string(os.PathSeparator)) {
		_ = os.RemoveAll(p.Stage)
	}
	result.State = "completed"
	result.Message = "程序已更新至 " + p.Version + "，配置已保留"
	result.Backup = backup
}
func restartUpdated(root string) *exec.Cmd {
	cmd := exec.Command(filepath.Join(root, "LinkWeaver.exe"), "--root", root, "--background")
	hideWindow(cmd)
	if cmd.Start() != nil {
		return nil
	}
	return cmd
}
