package db

import (
	"encoding/json"
	"time"
)

// LLM retry policy: Five-tier retry global configuration "number + interval", see docs/LLM retry
// design.md. A JSON value exists in the settings table - it is a machine-wide run parameter, not
// worth a table for it; Read goes to built-in defaults, so when keys do not exist (new
// library/never configured) behavior is perfectly consistent with write-dead constant era.

const settingLLMRetryPolicy = "llm_retry_policy"

// RetryRule is one layer's knob pair. The zero value means "unset":
//
//	Attempts 0 = Use the built-in default number of attempts; -1 = Shut down the layer and try
//	again; >0 = Use this value IntervalMS 0 = Use the original interval policy for the layer
//	(usually exponential avoidance); >0 = Use fixed millisecond intervals
//
// -1 is "explicit shutdown" and not "0 times", because 0 is already occupied by "unconfigured".
type RetryRule struct {
	Attempts   int `json:"attempts"`
	IntervalMS int `json:"interval_ms"`
}

// Interval returns the configured fixed interval, or 0 when unset (caller keeps
// its own default ladder).
func (r RetryRule) Interval() time.Duration {
	if r.IntervalMS <= 0 {
		return 0
	}
	return time.Duration(r.IntervalMS) * time.Millisecond
}

// Or returns the rule with each unset field filled in from fallback. Used to
// layer a profile override on top of the global policy field by field, so a
// profile that only pins the interval still inherits the global count.
func (r RetryRule) Or(fallback RetryRule) RetryRule {
	if r.Attempts == 0 {
		r.Attempts = fallback.Attempts
	}
	if r.IntervalMS == 0 {
		r.IntervalMS = fallback.IntervalMS
	}
	return r
}

// retry knob bounds. A count above the cap turns a blip into a token bonfire;
// an interval above an hour outlives any transient failure worth waiting out.
const (
	maxRetryAttempts   = 20
	maxRetryIntervalMS = 3600_000 // 1h
)

// Clamped returns the rule with out-of-range values pulled back into the sane
// band (attempts within [-1, 20], interval within [0, 1h]).
func (r RetryRule) Clamped() RetryRule {
	if r.Attempts < -1 {
		r.Attempts = -1
	}
	if r.Attempts > maxRetryAttempts {
		r.Attempts = maxRetryAttempts
	}
	if r.IntervalMS < 0 {
		r.IntervalMS = 0
	}
	if r.IntervalMS > maxRetryIntervalMS {
		r.IntervalMS = maxRetryIntervalMS
	}
	return r
}

// Clamped bounds a profile's override the same way the global policy is bounded,
// so a hand-crafted API payload can't land a value the CHECK constraint rejects.
func (o RetryOverride) Clamped() RetryOverride {
	o.Connect, o.Empty, o.Stream = o.Connect.Clamped(), o.Empty.Clamped(), o.Stream.Clamped()
	return o
}

// The LLMRetryPolicy holds the five-layer retry configuration. Connect/Empty/Stream are the
// per-request layers (a profile may override them, see LLMProfile.Retry); Breaker and Intent are
// process-wide by nature and live only here.
type LLMRetryPolicy struct {
	// Connect: SDK builds a connection retry (connection reset/timeout/429/5xx, before stream
	// starts). Default 3 times, index avoidance.
	Connect RetryRule `json:"connect"`
	// Empty: SDK empty response retry (complete but no content block, openai format only). Default
	// 2 times, index avoided.
	Empty RetryRule `json:"empty"`
	// Stream: Retry with provider's secure window (replay of the interrupted stream before the
	// output is not delivered). Default 2 times, index starting at 0.5s (peak 4s).
	Stream RetryRule `json:"stream"`
	// Breaker: Requests to melt. Attempts=Serial instantaneous failures trigger melt (default 3,
	// -1=no melt for instantaneous failure, hard failure if balance is low/key expires still melt
	// immediately); IntervalMS=Fixed cooldown time (0=default 1/5/30min gradient).
	Breaker RetryRule `json:"breaker"`
	// Intent: worker reruns the entire intent after model_error closes. Default 2 times, fixed 3s.
	Intent RetryRule `json:"intent"`
}

// Clamped returns the policy with every rule clamped.
func (p LLMRetryPolicy) Clamped() LLMRetryPolicy {
	p.Connect, p.Empty, p.Stream = p.Connect.Clamped(), p.Empty.Clamped(), p.Stream.Clamped()
	p.Breaker, p.Intent = p.Breaker.Clamped(), p.Intent.Clamped()
	return p
}

// LLMRetryPolicy reads the global retry policy. A missing or unparseable value
// yields the zero policy — i.e. every layer on its built-in default.
func (d *DB) LLMRetryPolicy() LLMRetryPolicy {
	var p LLMRetryPolicy
	if d == nil {
		return p
	}
	raw, ok, err := d.GetSetting(settingLLMRetryPolicy)
	if err != nil || !ok || raw == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return LLMRetryPolicy{}
	}
	return p.Clamped()
}

// SetLLMRetryPolicy persists the global retry policy (values are clamped first).
func (d *DB) SetLLMRetryPolicy(p LLMRetryPolicy) error {
	raw, err := json.Marshal(p.Clamped())
	if err != nil {
		return err
	}
	return d.SetSetting(settingLLMRetryPolicy, string(raw))
}
