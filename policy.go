package main

import (
	"math"
	"sort"
	"time"
)

type Sample struct {
	At     time.Time `json:"at"`
	MS     float64   `json:"ms"`
	OK     bool      `json:"ok"`
	Target string    `json:"target"`
}
type Route struct {
	GroupPriority        int       `json:"groupPriority"`
	GroupName            string    `json:"groupName"`
	Paused               bool      `json:"paused"`
	ID                   string    `json:"id"`
	Label                string    `json:"label"`
	Protocol             string    `json:"protocol"`
	Samples              []Sample  `json:"samples"`
	EWMA                 float64   `json:"ewma"`
	Jitter               float64   `json:"jitter"`
	Successes            int       `json:"successes"`
	Failures             int       `json:"failures"`
	ConsecutiveFailures  int       `json:"consecutiveFailures"`
	ConsecutiveSuccesses int       `json:"consecutiveSuccesses"`
	CheckedAt            time.Time `json:"checkedAt"`
	RetryAfter           time.Time `json:"retryAfter"`
	Score                float64   `json:"score"`
	Status               string    `json:"status"`
	SeedAvailable        bool      `json:"-"`
	Priority             int       `json:"priority"`
}

func (r *Route) Record(s Sample) {
	r.CheckedAt = s.At
	r.Samples = append(r.Samples, s)
	if len(r.Samples) > 24 {
		r.Samples = r.Samples[len(r.Samples)-24:]
	}
	if s.OK {
		r.Successes++
		r.ConsecutiveFailures = 0
		r.ConsecutiveSuccesses++
		r.RetryAfter = time.Time{}
		if r.EWMA == 0 {
			r.EWMA = s.MS
			r.Jitter = 0
		} else {
			d := math.Abs(s.MS - r.EWMA)
			r.EWMA = .65*r.EWMA + .35*s.MS
			r.Jitter = .7*r.Jitter + .3*d
		}
		r.Status = "available"
	} else {
		r.Failures++
		r.ConsecutiveFailures++
		r.ConsecutiveSuccesses = 0
		backoff := time.Duration(math.Min(600, 15*math.Pow(2, float64(min(r.ConsecutiveFailures, 6))))) * time.Second
		r.RetryAfter = s.At.Add(backoff)
		r.Status = "failed"
	}
	r.Score = r.score()
}

func (r *Route) score() float64 {
	if r.Successes == 0 {
		return 0
	}
	failed := 0
	for _, s := range r.Samples {
		if !s.OK {
			failed++
		}
	}
	return math.Round((r.EWMA+.5*r.Jitter+float64(failed)/float64(max(1, len(r.Samples)))*800)*10) / 10
}

func eligible(r *Route, now time.Time, c Config) bool {
	return !r.Paused && r.Successes > 0 && r.ConsecutiveFailures == 0 && now.Sub(r.CheckedAt) < time.Duration(max(120, c.IntervalSeconds*3))*time.Second
}

func ranked(routes map[string]*Route, now time.Time, c Config) []*Route {
	x := []*Route{}
	for _, r := range routes {
		if eligible(r, now, c) {
			x = append(x, r)
		}
	}
	sort.Slice(x, func(i, j int) bool {
		if !c.SpeedPriority && x[i].GroupPriority != x[j].GroupPriority {
			return x[i].GroupPriority < x[j].GroupPriority
		}
		if !c.SpeedPriority && x[i].Priority != x[j].Priority {
			return x[i].Priority < x[j].Priority
		}
		if x[i].Score == x[j].Score {
			return x[i].ID < x[j].ID
		}
		return x[i].Score < x[j].Score
	})
	return x
}

func choose(routes map[string]*Route, current string, lastSwitch, now time.Time, c Config) (string, string) {
	best := ranked(routes, now, c)
	if len(best) == 0 {
		return "", "没有近期验证通过的候选链路"
	}
	cur := routes[current]
	if current == "" || cur == nil {
		return best[0].ID, "首次选择完整链路可用的线路"
	}
	if cur.ConsecutiveFailures >= 2 {
		return best[0].ID, "当前链路连续两次失败，切换备用"
	}
	if !eligible(cur, now, c) {
		return current, "等待当前链路复测"
	}
	b := best[0]
	if b.ID == current {
		return current, "当前链路评分最优"
	}
	if now.Sub(lastSwitch) < time.Duration(c.MinDwellSeconds)*time.Second {
		return current, "驻留期内保持当前线路"
	}
	if b.ConsecutiveSuccesses < 2 {
		return current, "候选需要连续两次成功确认"
	}
	gain := cur.Score - b.Score
	if !c.SpeedPriority && (b.GroupPriority < cur.GroupPriority || b.GroupPriority == cur.GroupPriority && b.Priority < cur.Priority) {
		return b.ID, "更高优先级中转已恢复并通过连续验证"
	}
	if gain < float64(c.MinGainMS) || gain < cur.Score*float64(c.MinGainPercent)/100 {
		return current, "改善不足切换阈值"
	}
	return b.ID, "候选完整链路延迟持续更低"
}
