package main

import (
	"errors"
	"net/http"
	"slices"
)

type nodeBatchRequest struct {
	Action   string   `json:"action"`
	IDs      []string `json:"ids"`
	Priority *int     `json:"priority,omitempty"`
	Disabled *bool    `json:"disabled,omitempty"`
}

type nodeBatchResult struct {
	Changed int `json:"changed"`
	Skipped int `json:"skipped"`
}

// Validate the complete selection before changing anything. Retired cleanup is
// housekeeping; unlike an explicit deletion, it does not blacklist returning nodes.
func planNodeBatch(c Config, req nodeBatchRequest, protected map[string]bool) (Config, nodeBatchResult, error) {
	result := nodeBatchResult{}
	bad := func(message string) (Config, nodeBatchResult, error) { return c, result, errors.New(message) }
	if req.Action != "update" && req.Action != "delete" && req.Action != "clear-retired" {
		return bad("不支持的批量操作")
	}
	if req.Action == "clear-retired" && len(req.IDs) != 0 {
		return bad("清理退役节点不能指定其他节点")
	}
	if req.Action != "update" && (req.Priority != nil || req.Disabled != nil) {
		return bad("删除操作不能同时修改节点")
	}
	if req.Priority != nil && (*req.Priority < 1 || *req.Priority > 10000) {
		return bad("节点优先级范围 1–10000")
	}
	if req.Action == "update" && req.Priority == nil && req.Disabled == nil {
		return bad("请选择要修改的状态或优先级")
	}
	if len(req.IDs) > 2000 || (req.Action != "clear-retired" && len(req.IDs) == 0) {
		return bad("请先选择节点，单次最多 2000 个")
	}
	selected := map[string]bool{}
	for _, id := range req.IDs {
		selected[id] = true
	}
	for _, n := range c.Nodes {
		if req.Action == "clear-retired" && n.Retired {
			if protected[n.ID] {
				result.Skipped++
			} else {
				selected[n.ID] = true
			}
		}
	}
	known := map[string]bool{}
	for _, n := range c.Nodes {
		known[n.ID] = true
	}
	for id := range selected {
		if !known[id] {
			return bad("部分节点已变更，请刷新列表后重试；本次未修改任何节点")
		}
		if protected[id] && (req.Action != "update" || req.Disabled != nil && *req.Disabled) {
			return bad("选中节点仍被出口使用或手动锁定，请取消勾选或先切换中转；本次未修改任何节点")
		}
	}
	next := copyConfig(c)
	nodes := []Node{}
	changedIDs := map[string]bool{}
	for _, n := range next.Nodes {
		if !selected[n.ID] {
			nodes = append(nodes, n)
			continue
		}
		if req.Action != "update" {
			result.Changed++
			if req.Action == "delete" {
				key := proxyID(n.Proxy)
				if !slices.Contains(next.DeletedNodes, key) {
					next.DeletedNodes = append(next.DeletedNodes, key)
				}
			}
			continue
		}
		changed := false
		if req.Priority != nil && n.Priority != *req.Priority {
			n.Priority, changed = *req.Priority, true
		}
		if req.Disabled != nil && n.Disabled != *req.Disabled {
			n.Disabled, changed = *req.Disabled, true
		}
		if changed {
			changedIDs[n.ID] = true
		}
		nodes = append(nodes, n)
	}
	next.Nodes = nodes
	groups := []RelayGroup{}
	for _, g := range next.RelayGroups {
		members := []RelayGroupMember{}
		for _, m := range g.Members {
			if selected[m.NodeID] {
				if req.Action != "update" {
					continue
				}
				if req.Priority != nil && m.Priority != *req.Priority {
					m.Priority = *req.Priority
					if next.PoolMode == "custom" {
						changedIDs[m.NodeID] = true
					}
				}
			}
			members = append(members, m)
		}
		g.Members = members
		if len(members) > 0 {
			groups = append(groups, g)
		}
	}
	next.RelayGroups = groups
	if req.Action == "update" {
		result.Changed = len(changedIDs)
	}
	if req.Action != "update" {
		for i := range next.Profiles {
			next.Profiles[i].AllowedNodes = slices.DeleteFunc(next.Profiles[i].AllowedNodes, func(id string) bool { return selected[id] })
		}
	}
	return next, result, nil
}

func (w *Workspace) batchNodes(req nodeBatchRequest) (nodeBatchResult, error) {
	w.op.Lock()
	defer w.op.Unlock()
	c := w.snapshot()
	protected := map[string]bool{}
	for _, p := range c.Profiles {
		if p.ManualRelayID != "" {
			protected[p.ManualRelayID] = true
		}
	}
	w.mu.Lock()
	engines := []*Engine{}
	for _, e := range w.engines {
		engines = append(engines, e)
	}
	w.mu.Unlock()
	// Let any in-flight identity verification finish, then prevent automatic
	// selection from changing the protected set while committing the edit.
	for _, e := range engines {
		e.verifyMu.Lock()
		e.mu.Lock()
		e.editingNodes = true
		protected[e.current], protected[e.pinned] = true, true
		e.mu.Unlock()
		e.verifyMu.Unlock()
	}
	defer func() {
		for _, e := range engines {
			e.mu.Lock()
			e.editingNodes = false
			e.mu.Unlock()
		}
	}()
	next, result, err := planNodeBatch(c, req, protected)
	if err != nil || result.Changed == 0 {
		return result, err
	}
	if err = w.commit(next); err != nil {
		return nodeBatchResult{}, err
	}
	return result, nil
}

func (s *Server) relayBatch(w http.ResponseWriter, r *http.Request) {
	if !post(w, r) || !s.requireWorkspace(w) {
		return
	}
	var req nodeBatchRequest
	if err := decode(w, r, &req); err != nil {
		fail(w, err)
		return
	}
	result, err := s.workspace.batchNodes(req)
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, result)
}
