package alert

import "fmt"

// Silence suppresses the delivery and the demand-for-attention of matching
// alerts for a time window, without touching the rules or the evaluation. A
// silenced alert still fires, still advances its hysteresis and still records
// its transitions to the log — it is simply not posted onward and is shown as
// silenced rather than as shouting. That is the difference from an acknowledge,
// which mutes one firing episode's attention but keeps delivering: a silence is
// how a planned maintenance window, or a known issue already being worked, stops
// paging for a while without a lie about what is actually happening.
//
// A nil scope field is a wildcard. TargetID names a node and matches it and its
// whole subtree — the way every scope on this tree inherits — so silencing a
// group silences everything under it; AgentID and RuleID match exactly. StartsAt
// lets a window be booked ahead of time (a maintenance window); a silence that
// takes effect now is just one whose window has already opened.
type Silence struct {
	ID        int64  `json:"id"`
	TargetID  *int64 `json:"target_id"` // nil = every target; else this node and its subtree
	AgentID   *int64 `json:"agent_id"`  // nil = every agent
	RuleID    *int64 `json:"rule_id"`   // nil = every rule
	StartsAt  int64  `json:"starts_at"` // Unix seconds; the window is [StartsAt, EndsAt)
	EndsAt    int64  `json:"ends_at"`
	Reason    string `json:"reason"`
	CreatedBy string `json:"created_by"`
	CreatedAt int64  `json:"created_at"`
}

// activeAt reports whether the window covers t.
func (s *Silence) activeAt(t int64) bool {
	return t >= s.StartsAt && t < s.EndsAt
}

// Validate checks a silence is well formed before it is stored. Scope is not
// checked here — an unknown target or agent id simply matches nothing, which is
// a silence that does nothing rather than an error.
func (s *Silence) Validate() error {
	if s.EndsAt <= s.StartsAt {
		return fmt.Errorf("alert: silence must end after it starts")
	}
	return nil
}
