package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func batchFixture() Config {
	c := defaults()
	c.Sources = []RelaySource{{ID: "s", Name: "source", Enabled: true, Priority: 10}}
	for _, id := range []string{"live", "retired", "locked"} {
		c.Nodes = append(c.Nodes, Node{ID: id, Label: id, Priority: 5, Retired: id != "live", SourceID: "s", Proxy: map[string]any{"type": "socks5", "server": id + ".invalid", "port": 1080, "password": "keep-secret"}})
	}
	c.Profiles = []ExitProfile{{ID: "p", RestrictNodes: true, AllowedNodes: []string{"retired"}, ManualRelayID: "locked"}}
	c.RelayGroups = []RelayGroup{{ID: "g", Name: "old", Members: []RelayGroupMember{{NodeID: "retired", Priority: 5}}}}
	return c
}

func TestNodeBatchRetiredCleanupAndReturningSubscription(t *testing.T) {
	c := batchFixture()
	next, r, err := planNodeBatch(c, nodeBatchRequest{Action: "clear-retired"}, map[string]bool{"locked": true})
	if err != nil || r.Changed != 1 || r.Skipped != 1 || len(next.Nodes) != 2 || next.Nodes[0].ID != "live" {
		t.Fatalf("wrong cleanup: %+v %v", r, err)
	}
	if len(next.DeletedNodes) != 0 || len(next.RelayGroups) != 0 || len(next.Profiles[0].AllowedNodes) != 0 || !next.Profiles[0].RestrictNodes {
		t.Fatal("cleanup left references, blacklisted nodes or broadened restricted profile")
	}
	if len(c.Nodes) != 3 || len(c.RelayGroups[0].Members) != 1 || len(c.Profiles[0].AllowedNodes) != 1 {
		t.Fatal("planning mutated the original config")
	}
	back := mergeSourceNodes(next, next.Sources[0], []map[string]any{c.Nodes[1].Proxy})
	found := false
	for _, n := range back.Nodes {
		if n.Proxy["server"] == "retired.invalid" && !n.Retired {
			found = true
		}
	}
	if !found {
		t.Fatal("retired cleanup prevented a legitimately returning subscription node")
	}
}

func TestNodeBatchDeleteKeepsSubscriptionExclusion(t *testing.T) {
	c := batchFixture()
	next, r, err := planNodeBatch(c, nodeBatchRequest{Action: "delete", IDs: []string{"retired", "retired"}}, nil)
	if err != nil || r.Changed != 1 || len(next.DeletedNodes) != 1 {
		t.Fatalf("duplicate IDs or deletion exclusion mishandled: %+v %v", r, err)
	}
	back := mergeSourceNodes(next, next.Sources[0], []map[string]any{c.Nodes[1].Proxy})
	for _, n := range back.Nodes {
		if n.Proxy["server"] == "retired.invalid" {
			t.Fatal("explicitly deleted node returned after refresh")
		}
	}
}

func TestNodeBatchRejectsWholeInvalidSelection(t *testing.T) {
	c := batchFixture()
	disabled, invalidPriority := true, 0
	for _, req := range []nodeBatchRequest{
		{Action: "delete", IDs: []string{"retired", "live"}},
		{Action: "update", IDs: []string{"retired", "live"}, Disabled: &disabled},
		{Action: "delete", IDs: []string{"retired", "missing"}},
		{Action: "update", IDs: []string{"retired"}, Priority: &invalidPriority},
		{Action: "delete"},
		{Action: "clear-retired", IDs: []string{"live"}},
		{Action: "update", IDs: []string{"retired"}},
	} {
		got, r, err := planNodeBatch(c, req, map[string]bool{"live": true})
		if err == nil || r.Changed != 0 || !reflect.DeepEqual(c, got) {
			t.Fatalf("invalid batch was partially applied: %+v %v", req, err)
		}
	}
}

func TestNodeBatchCustomPriorityAndSecrets(t *testing.T) {
	c := batchFixture()
	c.PoolMode = "custom"
	c.RelayGroups[0].Members[0].Priority = 1
	priority := 5
	next, r, err := planNodeBatch(c, nodeBatchRequest{Action: "update", IDs: []string{"retired"}, Priority: &priority}, map[string]bool{"retired": true})
	if err != nil || r.Changed != 1 || next.RelayGroups[0].Members[0].Priority != 5 {
		t.Fatalf("custom group priority ignored: %+v %v", r, err)
	}
	before, _ := json.Marshal(c.Nodes[1].Proxy)
	after, _ := json.Marshal(next.Nodes[1].Proxy)
	if string(before) != string(after) || !next.Nodes[1].Retired {
		t.Fatal("batch edit changed credentials or revived retired node")
	}
}

func TestNodeBatchSelectionBarrier(t *testing.T) {
	e := &Engine{state: "running", editingNodes: true, current: "live"}
	if err := e.trySelect(context.Background(), "other", "test", false); err != nil || e.current != "live" {
		t.Fatal("automatic selection bypassed edit barrier")
	}
}
