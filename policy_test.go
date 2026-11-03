package main

import (
	"testing"
	"time"
)

func healthy(id string, ms float64, now time.Time) *Route {
	r := &Route{ID: id, Status: "unknown"}
	r.Record(Sample{At: now.Add(-time.Second), MS: ms, OK: true})
	r.Record(Sample{At: now, MS: ms, OK: true})
	return r
}
func TestSelectionHysteresisAndEmergency(t *testing.T) {
	c := defaults()
	now := time.Now()
	a := healthy("a", 400, now)
	b := healthy("b", 200, now)
	rs := map[string]*Route{"a": a, "b": b}
	if id, _ := choose(rs, "a", now.Add(-time.Second), now, c); id != "a" {
		t.Fatal("must honor dwell")
	}
	if id, _ := choose(rs, "a", now.Add(-5*time.Minute), now, c); id != "b" {
		t.Fatal("must select materially faster confirmed path")
	}
	b.EWMA = 360
	b.Score = b.score()
	if id, _ := choose(rs, "a", now.Add(-5*time.Minute), now, c); id != "a" {
		t.Fatal("must honor both gain thresholds")
	}
	b.EWMA = 200
	b.Score = b.score()
	a.Record(Sample{At: now, OK: false})
	if id, _ := choose(rs, "a", now.Add(-time.Second), now, c); id != "a" {
		t.Fatal("single failure must not cause flapping")
	}
	a.Record(Sample{At: now, OK: false})
	if id, _ := choose(rs, "a", now.Add(-time.Second), now, c); id != "b" {
		t.Fatal("two failures must bypass dwell")
	}
}
func TestNeverSelectStaleOrFailed(t *testing.T) {
	c := defaults()
	now := time.Now()
	a := healthy("a", 30, now.Add(-10*time.Minute))
	b := healthy("b", 80, now)
	b.Record(Sample{At: now, OK: false})
	if id, _ := choose(map[string]*Route{"a": a, "b": b}, "", time.Time{}, now, c); id != "" {
		t.Fatal("stale and failed routes cannot be selected")
	}
}
func TestFailureBackoffAndRecovery(t *testing.T) {
	now := time.Now()
	r := healthy("a", 100, now)
	r.Record(Sample{At: now, OK: false})
	first := r.RetryAfter
	r.Record(Sample{At: now, OK: false})
	if !r.RetryAfter.After(first) {
		t.Fatal("backoff must grow")
	}
	r.Record(Sample{At: now, MS: 110, OK: true})
	if !r.RetryAfter.IsZero() || r.ConsecutiveFailures != 0 || r.ConsecutiveSuccesses != 1 {
		t.Fatal("success must reset backoff")
	}
	if len(r.Samples) != 5 {
		t.Fatal("history lost")
	}
}
func TestSingleTargetTwoSuccessesRequired(t *testing.T) {
	c := defaults()
	now := time.Now()
	a := healthy("a", 600, now)
	b := &Route{ID: "b"}
	b.Record(Sample{At: now, MS: 100, OK: true})
	if id, _ := choose(map[string]*Route{"a": a, "b": b}, "a", now.Add(-time.Hour), now, c); id != "a" {
		t.Fatal("one lucky probe cannot trigger a switch")
	}
}
