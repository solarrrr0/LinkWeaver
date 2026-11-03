package main

import (
	"errors"
	"slices"
)

type RelayGroupMember struct {
	NodeID   string `json:"nodeID" yaml:"nodeID"`
	Priority int    `json:"priority" yaml:"priority"`
}
type RelayGroup struct {
	ID       string             `json:"id" yaml:"id"`
	Name     string             `json:"name" yaml:"name"`
	Priority int                `json:"priority" yaml:"priority"`
	Paused   bool               `json:"paused" yaml:"paused"`
	Members  []RelayGroupMember `json:"members" yaml:"members"`
}

func nodeSources(n Node) []string {
	if len(n.SourceIDs) > 0 {
		return n.SourceIDs
	}
	if n.SourceID != "" {
		return []string{n.SourceID}
	}
	return nil
}
func setNodeSources(n *Node, ids []string) {
	n.SourceIDs = slices.Clone(ids)
	n.SourceID = ""
	if len(ids) > 0 {
		n.SourceID = ids[0]
	}
}
func sourceNode(c Config, n Node, allowed []string) (bool, int, string, bool) {
	found := false
	priority := 10000
	name := "未分组备用"
	paused := true
	for _, id := range nodeSources(n) {
		if len(allowed) > 0 && !slices.Contains(allowed, id) {
			continue
		}
		for _, s := range c.Sources {
			if s.ID == id && s.Enabled {
				found = true
				if !s.Paused && (paused || s.Priority < priority) {
					priority = s.Priority
					name = s.Name
					paused = false
				} else if paused && s.Priority < priority {
					priority = s.Priority
					name = s.Name
				}
			}
		}
	}
	return found, priority, name, paused
}
func applyGroup(c Config, n Node) Node {
	_, priority, name, paused := sourceNode(c, n, nil)
	n.GroupPriority = priority
	n.GroupName = name
	n.Paused = paused
	if c.PoolMode == "custom" {
		n.GroupPriority = 10000
		n.GroupName = "未分组备用"
		n.Paused = false
		for _, g := range c.RelayGroups {
			for _, m := range g.Members {
				if m.NodeID == n.ID {
					n.GroupPriority = g.Priority
					n.GroupName = g.Name
					n.Priority = m.Priority
					n.Paused = g.Paused
					return n
				}
			}
		}
	}
	return n
}
func validateGroups(c Config) error {
	if c.PoolMode != "" && c.PoolMode != "source" && c.PoolMode != "custom" {
		return errors.New("分组方式应为按来源或自定义")
	}
	if len(c.RelayGroups) > 20 {
		return errors.New("最多支持 20 个中转分组")
	}
	ids, names, nodes := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, g := range c.RelayGroups {
		if g.ID == "" || ids[g.ID] || g.Name == "" || len([]rune(g.Name)) > 100 || names[g.Name] || g.Priority < 0 || g.Priority > 10000 {
			return errors.New("分组名称或优先级无效，名称不能重复")
		}
		ids[g.ID] = true
		names[g.Name] = true
		if len(g.Members) == 0 {
			return errors.New("每个分组至少选择一个中转")
		}
		for _, m := range g.Members {
			if m.NodeID == "" || nodes[m.NodeID] || m.Priority < 1 || m.Priority > 10000 {
				return errors.New("节点不能重复分组，节点优先级范围 1–10000")
			}
			nodes[m.NodeID] = true
		}
	}
	return nil
}
func (w *Workspace) saveGroups(mode string, groups []RelayGroup) error {
	w.op.Lock()
	defer w.op.Unlock()
	c := w.snapshot()
	for i := range groups {
		if groups[i].ID == "" {
			groups[i].ID = randomToken()[:12]
		}
	}
	c.PoolMode = mode
	c.RelayGroups = groups
	return w.commit(c)
}
func (w *Workspace) deleteGroup(id string, deleteNodes bool) error {
	w.op.Lock()
	defer w.op.Unlock()
	c := w.snapshot()
	members := []string{}
	out := []RelayGroup{}
	for _, g := range c.RelayGroups {
		if g.ID == id {
			for _, m := range g.Members {
				members = append(members, m.NodeID)
			}
		} else {
			out = append(out, g)
		}
	}
	if deleteNodes {
		for _, e := range w.engines {
			e.mu.Lock()
			used := slices.Contains(members, e.current) || slices.Contains(members, e.pinned)
			e.mu.Unlock()
			if used {
				return errors.New("该组仍被出口使用，请先切换当前中转")
			}
		}
		nodes := []Node{}
		for _, n := range c.Nodes {
			if slices.Contains(members, n.ID) {
				c.DeletedNodes = append(c.DeletedNodes, proxyID(n.Proxy))
			} else {
				nodes = append(nodes, n)
			}
		}
		c.Nodes = nodes
	}
	c.RelayGroups = out
	return w.commit(c)
}
