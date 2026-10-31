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

const plannerWrapUpDefault = "你本轮规划的步数即将用尽——注意只是【这一轮】结束,系统之后仍会随态势变化再次唤醒你继续规划,并非任务终止,你无需在此收束整个规划。请把本轮已经想清楚的结论落地、别让这一轮白跑,但也【不要为了收尾硬凑意图】(本轮 0 个意图仍是完全正常的结果)：(1) 若已判断出【当前就该派发】的探索方向,用一次 add_intent 批量提交(想好的别憋着不发);(2) 对已被某发现/事实证明达成的目标,调 prove_goal 标记 met(别漏判);(3) 若识别出需要分步的串行利用链,用 TodoWrite 记下,便于下次唤醒接着派。做完直接结束本轮,无需输出总结文本。"

const mainAgentWrapUpDefault = "你的步数即将用尽,本次交互就要结束。不要再发起新的探索/操作。请**单独用一句话纯文本**向用户总结当前进展、关键结论,以及建议的下一步。"

const genericWrapUpDefault = "你即将因预算耗尽被终止。请先把已完成但未落库的结果写回,再**单独用一句话纯文本**总结你做了什么、得到哪些关键结论(这句会作为本次运行的结果展示)。"

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

const workerTaskTimeoutDefault = "**整个任务已到达超时上限，即将结束**（不是你这次 run 的预算，是整场探索到点了）。这是最后机会：(1) 把你已识别但还没写回的内容【全部】落库——新资产 insert_assets、探索结论/事实 record_fact、确认漏洞 report_finding；(2) 不要再启动任何新命令/探测；(3) **最后单独用一句话纯文本**总结你在本意图上的关键结论。"

const plannerTaskTimeoutDefault = "**整个任务已到达超时上限，即将结束**（不是本轮，是整个任务终止）。请基于当前【全部】事实与发现，做最后一次目标判定：对已被证据证明达成的目标调 prove_goal 标记 met（别漏判）。**不要再生成任何新意图**（此时派意图也不会再被执行）。判定完即收束，无需输出总结文本。"

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
