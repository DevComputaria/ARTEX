package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// For the service-side analysis of the retry policy, see docs/LLM Retry Design.md.
// - the built-in link / empty response / same provider security window is "follows the endpoint",
// and each LLM configuration can override the global default (a blank entry in profile inherits the
// global, and a global without an empty entry uses the built-in default);
// - Melting/intent to rerun is process level, only one global copy.
//
// Global policies read the DB settings line once, and call points are all on low-frequency paths
// (build provider, work end, save configuration), so it is not worth adding another cache layer;
// the melt parameter is an exception—it is read every time on failed paths, so it is pushed to the
// registry by applyRetryPolicy.

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// The original semantics of "0=default / negative=off" is kept here: the MaxRetries /
		// EmptyResponseRetries of the SDK are completely isomorphic with it, so leave it to parse
		// itself.
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// The default value for the meltdown (request cooldown) is the same as the built-in llmpool, which
// is overridden here only when "user has assigned a value". For the default value for retrying, see
// modelErrorRetries / modelErrorRetryBackoff in engine.go.

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). It deliberately reuses layer ②'s knob ─ "Number of empty response
// retries": Both are two ways of doing the same thing. The SDK layer does "no content block" by
// re-sending the same request as it is; here it does "just think, no body, no tools", by adding an
// instruction to the model to do it with the thought already in it (the re-sending does not make
// sense for this context-determined empty turn).
//
// Read global policy rather than override a particular profile: A run may fail to switch profiles,
// which is the total lock of the entire intent and should not change with the switching endpoint.
// Semantically identical to the SDK's emptyRetries(): 0 = default defaultEmptyTurnNudges; -1
// (negative) = turn off empty turn retries; >0 = use this value.
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
