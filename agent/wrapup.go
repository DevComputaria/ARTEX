package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// Wrap-up / settlement prompt: When the agent is terminated due to MaxTurns or
// run_seconds/MaxDuration, the SDK's settlement phase injects this prompt to allow the agent to
// wrap up the identified but unwritten content and output a summary to avoid a mess.
//
// The end prompt for each agent can be overridden in the background as needed (save
// agents.wrapup_prompt), or left blank with the built-in default here. Only the [Prompt body] can
// be edited; which tools to disable, the end itself, and the policy for a few rounds of budgeting
// are fixed by code.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// Built-in default end prompt, indexed by agent key.worker uses the hard-coded settleWrapUpPrompt
// (defined in worker.go) from the past, planner/mainagent have one version each; uncommanded
// (custom agent) go universal.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: The round budget for each agent's end stage is built-in by default (can be
// overridden by the backend >0). Gives 10 rounds to ensure that there are enough turns left in the
// end stage.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "You're about to run out of the number of steps you have planned for this round - just remember that [this round] is over, and the system will wake you up again as the situation changes, not terminate the task, so you don't have to end the whole planning here. Please keep your decisions clear and don'ts let this round go unfinished, but also [don't make intents to finish it] (0 intents in this round are still perfectly normal): (1) If you have decided on the direction of exploration [currently to be distributed], batch submit with one add_intent (do not hold back if you want to); (2) Mark met with prove_goal for a finding/fact that has already been achieved; (3) If identified sequence use chains requiring steps, write them down with TodoWrite so they can be used next time you wake up."

const mainAgentWrapUpDefault = "Your steps are running out, and this interaction is about to end. No more new exploration/operations. ** Please summarize the current progress, key conclusions and suggested next steps to users in a single sentence in plain text**."

const genericWrapUpDefault = "You are about to be terminated by the depletion of your budget. Please write back on the results that have been completed but not stored, and then ** in a single plain text to summarize what you have done and which key conclusions you have drawn (which will be shown as the result of this operation)."

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- Task-level timeout endings (see docs/task-level-timeout-and-end-design.md)----------
//
// The endings of per-run are [two sets]: per-run is "your budget for this run has been used up";
// task overtime is "the whole task is about to end at a point". The semantics are often reversed
// (especially planner: per-run says "do not stop planning", task overtime says "stop planning at a
// point, make the final decision").

// WrapupTaskTimeoutOverride / …TurnsOverride: DB override of task timeout ending and turn count
// (wire to agents.task_timeout_wrapup_prompt / _max_turns, worker/planner only).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**The whole task has reached its timeout limit and is about to end** (not your run budget, but the entire exploration). This is your last chance: (1) to archive everything you've identified that hasn't been written back—new assets insert_assets, findings/facts record_fact, confirmed vulnerabilities report_finding; (2) not to start any new commands/detections; and (3) to summarize your key findings on this intent in a single sentence of plain text."

const plannerTaskTimeoutDefault = "**The whole task has reached the timeout limit and is about to end** (not this round, but the entire task). Based on the current [all] facts and findings, please make a final decision: mark met for the goals that have been proven by evidence. **Do not generate any new intents** (the intent will then no longer be executed)."

// TaskTimeoutWrapupDefault Returns the built-in default wrap for a task timeout of an agent (for
// backend takeover/reset).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // Unconfigured (mainagent/chat) returns an empty string
}

// resolveTaskTimeoutWrapup: DB override (non-empty) > built-in default. Empty string indicates that
// the agent has no task timeout word (not worker/planner), and the caller should return the per-run
// word.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // default Follows per-run number
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
// - clamped=true → This run is forced by the task deadline: because of Timeout end = task to point
// → task timeout word; because of MaxTurns end = forced window steps are exhausted, task remaining
// minutes → fall back per-run word.
// - clamped=false → Task is too early: Both reasons use per-run words (i.e. degrade to
// wrapupSettlement).
//
// The PromptByReason given to harness is picked on the spot by reason at the end, and mismatched
// when no build is performed.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // (also the value of two reasons when not clamped)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // Mission to Point
				harness.ReasonMaxTurns: perRun, // Steps are running out, time remains
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
