package main

import (
	"testing"
	"time"
)

func TestJobResultsExpireAndDoNotAccumulate(t *testing.T) {
	now := time.Now()
	w := &Workspace{jobs: map[string]*Job{}}
	for i := 0; i < 100; i++ {
		id := time.Unix(int64(i), 0).String()
		w.jobs[id] = &Job{ID: id, Kind: "pool-check", State: "completed", At: now.Add(time.Duration(i) * time.Millisecond), FinishedAt: now}
	}
	w.jobs["refresh"] = &Job{ID: "refresh", Kind: "refresh", State: "running", At: now.Add(-time.Hour)}
	w.jobs["failure"] = &Job{ID: "failure", Kind: "measure", State: "failed", At: now, FinishedAt: now.Add(-10 * time.Second)}
	w.pruneJobsLocked(now)
	if len(w.jobs) != 3 {
		t.Fatalf("repeated completed tasks accumulated: %d", len(w.jobs))
	}
	w.pruneJobsLocked(now.Add(6 * time.Second))
	if len(w.jobs) != 1 || w.jobs["refresh"] == nil {
		t.Fatal("results did not expire or running task removed")
	}
}

func TestRunningJobReplacesPreviousResultOfSameKind(t *testing.T) {
	now := time.Now()
	w := &Workspace{jobs: map[string]*Job{
		"old":    {ID: "old", Kind: "refresh", State: "completed", At: now, FinishedAt: now},
		"active": {ID: "active", Kind: "refresh", State: "running", At: now.Add(time.Second)},
	}}
	got := w.jobViews()
	if len(got) != 1 || got[0].ID != "active" {
		t.Fatal("old result shown beside new task")
	}
}
