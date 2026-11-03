package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"time"
)

type Job struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Title      string    `json:"title"`
	State      string    `json:"state"`
	Completed  int       `json:"completed"`
	Total      int       `json:"total"`
	Failed     int       `json:"failed"`
	Message    string    `json:"message"`
	At         time.Time `json:"at"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	cancel     context.CancelFunc
}

func jobResultLifetime(state string) time.Duration {
	if state == "failed" {
		return 15 * time.Second
	}
	return 6 * time.Second
}

// Keep active work and a short result notice; the dashboard is not a task history.
// Caller holds w.mu.
func (w *Workspace) pruneJobsLocked(now time.Time) {
	latest := map[string]*Job{}
	for id, j := range w.jobs {
		if j.State != "running" && (j.FinishedAt.IsZero() || !now.Before(j.FinishedAt.Add(jobResultLifetime(j.State)))) {
			delete(w.jobs, id)
			continue
		}
		old := latest[j.Kind]
		if old == nil {
			latest[j.Kind] = j
			continue
		}
		if j.State == "running" && old.State != "running" || j.State == old.State && j.At.After(old.At) || old.State != "running" && j.State != "running" && j.At.After(old.At) {
			delete(w.jobs, old.ID)
			latest[j.Kind] = j
		} else {
			delete(w.jobs, id)
		}
	}
}

func (w *Workspace) startJob(kind, title string, run func(context.Context, *Job) error) (string, error) {
	w.mu.Lock()
	if w.closing {
		w.mu.Unlock()
		return "", errors.New("后台正在退出")
	}
	if w.jobs == nil {
		w.jobs = map[string]*Job{}
	}
	w.pruneJobsLocked(time.Now())
	for _, j := range w.jobs {
		if j.Kind == kind && j.State == "running" {
			w.mu.Unlock()
			return "", errors.New("同类任务已在进行")
		}
	}
	for id, j := range w.jobs {
		if j.Kind == kind {
			delete(w.jobs, id)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{ID: randomToken()[:12], Kind: kind, Title: title, State: "running", At: time.Now(), cancel: cancel}
	w.jobs[j.ID] = j
	w.mu.Unlock()
	go func() {
		err := run(ctx, j)
		w.mu.Lock()
		defer w.mu.Unlock()
		j.State = "completed"
		if err != nil {
			j.State = "failed"
			j.Message = err.Error()
		}
		if ctx.Err() != nil {
			j.State = "cancelled"
			j.Message = "任务已取消"
		}
		j.FinishedAt = time.Now()
		cancel()
	}()
	return j.ID, nil
}
func (w *Workspace) progress(j *Job, completed, total, failed int, message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	j.Completed = completed
	j.Total = total
	j.Failed = failed
	j.Message = message
}
func (w *Workspace) jobViews() []Job {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneJobsLocked(time.Now())
	out := []Job{}
	for _, j := range w.jobs {
		out = append(out, *j)
	}
	slices.SortFunc(out, func(a, b Job) int { return b.At.Compare(a.At) })
	return out
}
func (w *Workspace) cancelJob(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if j := w.jobs[id]; j != nil && j.State == "running" {
		j.cancel()
	}
}
func (w *Workspace) cancelBackground() {
	w.mu.Lock()
	w.closing = true
	for _, j := range w.jobs {
		j.cancel()
	}
	w.mu.Unlock()
	w.poolMu.Lock()
	if w.pool != nil {
		w.pool.Stop()
	}
	w.poolMu.Unlock()
}

func (w *Workspace) refreshJob(ids []string) (string, error) {
	return w.startJob("refresh", "刷新订阅来源", func(ctx context.Context, j *Job) error {
		sources := []RelaySource{}
		for _, s := range w.snapshot().Sources {
			if s.Enabled && s.URL != "" && (len(ids) == 0 || slices.Contains(ids, s.ID)) {
				sources = append(sources, s)
			}
		}
		failed := 0
		for i, s := range sources {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.progress(j, i, len(sources), failed, "正在刷新来源")
			if err := w.refreshSourceContext(ctx, s.ID); err != nil {
				failed++
			}
			w.progress(j, i+1, len(sources), failed, fmt.Sprintf("已处理 %d 个来源，失败 %d 个", i+1, failed))
		}
		if failed > 0 {
			return fmt.Errorf("%d 个来源刷新失败，已保留旧节点", failed)
		}
		return nil
	})
}
func (w *Workspace) batchMeasureJob() (string, error) {
	return w.startJob("measure", "检测全部运行出口", func(ctx context.Context, j *Job) error {
		profiles := []ExitProfile{}
		for _, p := range w.snapshot().Profiles {
			e, _ := w.engine(p.ID)
			if e == nil {
				continue
			}
			e.mu.Lock()
			ready := e.state == "running" && e.current != ""
			e.mu.Unlock()
			if ready {
				profiles = append(profiles, p)
			}
		}
		failed := 0
		for i, p := range profiles {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			e, _ := w.engine(p.ID)
			if _, err := e.Measure(); err != nil {
				failed++
			}
			w.progress(j, i+1, len(profiles), failed, "出口检测进行中")
		}
		if failed > 0 {
			return fmt.Errorf("检测完成，%d 个出口失败", failed)
		}
		return nil
	})
}

func (w *Workspace) poolCheckJob(ids []string) (string, error) {
	return w.startJob("pool-check", "独立检测中转池", func(ctx context.Context, j *Job) error {
		w.poolMu.Lock()
		defer w.poolMu.Unlock()
		c := w.snapshot()
		nodes := []Node{}
		for _, n := range c.Nodes {
			if !n.Disabled && !n.Retired {
				enabled, _, _, _ := sourceNode(c, n, nil)
				if enabled {
					nodes = append(nodes, applyGroup(c, n))
				}
			}
		}
		if len(nodes) == 0 {
			return errors.New("没有可检测的中转节点")
		}
		total := 0
		for _, n := range nodes {
			if len(ids) == 0 || slices.Contains(ids, n.ID) {
				total++
			}
		}
		if total == 0 {
			return errors.New("所选中转已停用或退役")
		}
		c.Nodes = nodes
		c.FixedExit = nil
		c.Profiles = nil
		c.Sources = nil
		c.Targets = c.Targets[:1]
		c.ManualRelayID = ""
		c.SelectionMode = "automatic"
		c.ProbeOnly = true
		c.ProxyPort = 29871
		c.VerifyPort = 29872
		w.mu.Lock()
		e := w.pool
		w.mu.Unlock()
		if e != nil {
			old := e.cfg()
			c.ControllerPort = old.ControllerPort
			if !sameCore(old, c) {
				e.Stop()
				e = nil
			}
		}
		if e == nil {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return errors.New("无法分配检测端口")
			}
			c.ControllerPort = listener.Addr().(*net.TCPAddr).Port
			listener.Close()
			if c.ControllerPort == c.ProxyPort || c.ControllerPort == c.VerifyPort || c.ControllerPort == c.UIPort {
				return errors.New("检测端口冲突，请重试")
			}
			e = newEngine(w.root, c)
			e.scope = "pool-health"
			w.mu.Lock()
			w.pool = e
			w.mu.Unlock()
		}
		e.mu.Lock()
		running := e.state == "running"
		// Detection settings can change without dropping existing TUIC/QUIC sessions.
		e.config = c
		e.mu.Unlock()
		if !running {
			e.Stop()
			if err := e.Start(); err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.progress(j, 0, total, 0, "检测中转，复用现有检测连接")
		progressDone := make(chan struct{})
		go func() {
			ticker := time.NewTicker(150 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-progressDone:
					return
				case <-ticker.C:
					e.mu.Lock()
					done, total := e.checked, e.total
					e.mu.Unlock()
					w.progress(j, done, total, 0, "只检查中转本身，不经过固定出口")
				}
			}
		}()
		err := e.scan(ctx, true, ids)
		close(progressDone)
		if err == nil {
			w.progress(j, total, total, 0, "中转检测完成")
		}
		return err
	})
}

func (w *Workspace) setSelection(id, node string) error {
	w.op.Lock()
	defer w.op.Unlock()
	e, err := w.engine(id)
	if err != nil {
		return err
	}
	e.mu.Lock()
	running := e.state == "running"
	allowed := node == ""
	for _, n := range e.config.Nodes {
		if n.ID == node {
			allowed = true
		}
	}
	e.mu.Unlock()
	if !allowed {
		return errors.New("所选节点不在此出口的候选池中")
	}
	if running {
		if err = e.ManualSelect(node); err != nil {
			return err
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	c := copyConfig(w.config)
	for i := range c.Profiles {
		if c.Profiles[i].ID == e.scope {
			c.Profiles[i].ManualRelayID = node
			c.Profiles[i].SelectionMode = "automatic"
			if node != "" {
				c.Profiles[i].SelectionMode = "manual"
			}
		}
	}
	if err = saveConfig(filepath.Join(w.root, "data", "config.json"), c); err != nil {
		return errors.New("选择已应用，但保存失败")
	}
	w.config = c
	e.mu.Lock()
	e.config.SelectionMode = "automatic"
	e.config.ManualRelayID = node
	e.pinned = node
	e.automatic = node == ""
	if node != "" {
		e.config.SelectionMode = "manual"
	}
	e.mu.Unlock()
	return nil
}

func (w *Workspace) backgroundLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	nextRefresh := time.Now()
	nextCheck := time.Now()
	retry := map[string]time.Time{}
	failures := map[string]int{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			c := w.snapshot()
			if c.AutoRefresh && !now.Before(nextRefresh) {
				_, _ = w.refreshJob(nil)
				nextRefresh = now.Add(time.Duration(c.RefreshMinutes) * time.Minute)
			}
			if !now.Before(nextCheck) {
				_, _ = w.poolCheckJob(nil)
				nextCheck = now.Add(time.Duration(c.IntervalSeconds) * time.Second)
			}
			if !c.AutoRecover {
				continue
			}
			for _, p := range c.Profiles {
				if ctx.Err() != nil {
					return
				}
				if !p.Running || now.Before(retry[p.ID]) {
					continue
				}
				e, _ := w.engine(p.ID)
				if e == nil {
					continue
				}
				e.mu.Lock()
				state := e.state
				e.mu.Unlock()
				if state != "error" {
					if state == "running" {
						failures[p.ID] = 0
					}
					continue
				}
				w.op.Lock()
				latest := w.snapshot()
				desired := false
				for _, item := range latest.Profiles {
					if item.ID == p.ID {
						desired = item.Running
					}
				}
				current, _ := w.engine(p.ID)
				e.mu.Lock()
				stillError := e.state == "error"
				e.mu.Unlock()
				if !desired || current != e || !stillError {
					w.op.Unlock()
					continue
				}
				e.Stop()
				err := e.Start()
				w.op.Unlock()
				if err != nil {
					failures[p.ID]++
					retry[p.ID] = now.Add(time.Duration(min(60, 5*(1<<min(failures[p.ID], 4)))) * time.Second)
					e.mu.Lock()
					e.state = "error"
					e.lastError = "核心恢复失败，稍后重试"
					e.mu.Unlock()
				} else {
					e.event("代理核心已自动恢复，等待链路及出口校验")
				}
			}
		}
	}
}
