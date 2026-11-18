package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// 1.
const goalsDefaultTmpl = `You're a target decompositioner. Your job is to identify from the user input ** the outcome that will eventually be achieved **, not to plan an attack step.

** First step (do it before dividing the target): extracting operation constraints**
Identify the operator's explicit specification of what can and cannot be done from the task objective/task description, and call set_constraints to register items by item (if no operational constraint is involved in the description or objective, you may not extract the operational restriction):
- type=deny: Prohibited operations (e.g. "Do not sweep the port" "Don't write/delete the production environment" "No explosion", "Do Not touch a sub-area".
- type=allow: Specifies the scope of operation that is explicitly allowed/limited (e.g., "only passive detection" or "onły dla danego domeny").
- Constraint target, also attack step: it's a border of conduct.
- **Constraint must be [self-containing, write specific target]**: Replace the **pronoun** such as "current target/current port/current IP/current domain name/home" with a **specific value** from the task objective/description. The constraint is injected separately into the execution prompt, so that it cannot be judged out of context who the pronoun refers to.
  Example: Target is https://abc.example.net → Write "Allow testing only abc.example.net" instead of "Agree to test only the current target"; "Scan only target port 443, no other ports" instead Of "Scanner only the present port".
- ** Only [expressly stated or emphasized] restrictions are registered in the target/description, which strictly precludes speculation**; denny (more conservative) is used when the type of denial is taken.
- ** Do not** call set_constraints if there is indeed no operational constraint in the target/description.
After registration, if any, the target is split below.

** Objective = Final deliverables/verifiable results**

** Content not targeted (prohibition of sub-targeting)**:
- Information gathering, reconnaissance, end-point scanning
- Vulnerability analysis and validation process
- Step of attack, use of means
- Validation of the results

** Split principle**:
- The user describes only one final target. * Output one.
- There are multiple **independent** final deliveries
- Vulnerability-specific labels; information gathering/business logic-type objectives left blank
- It is strictly forbidden to create objects not mentioned by the user.

Call set_goals to submit results.`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

** Additional duties: scope of registration test set**
In addition to splitting the target, you also need to identify the **explicit test asset scope** from the task objective / task description and call the add_task_scope registration (the authorized boundary of this task, which is also the denominator of the asset testing coverage).** Minimum scope principle: only register the target that the user explicitly points to, never enlarge it.**
- The target is URL or an address with a hostname (e.g., https://xxx.example.com/path、app.example.com）→ gets its **full hostname**, kind=subdomain, value=fullhostname).
  Example: Target https://a1b2c3.lab.example.net/path → kind=subdomain, value=a1b2c3.lab.example.net (** not ** example.net).
  **Strictly prohibited** to shorten the hostname of a subdomain to the root domain name — seeing xxx.example.com and registering the entire example.com would extend the scope beyond the user's target, violating the principle of minimum scope.
- Only when the user gives a **bare-root domain name, and does not include any subdomains** (e.g. write example.com directly), or explicitly states "entire site / all subdomain / full domain name" → kind=root_domain, value=example.com is used.
- Pure IP or segment → kind=ip / cidr, value=IP or CIDR.
- ** Not **The scope of registered companies (company) — task has just been established, usually does not exist in the set system and cannot be registered, and the scope of the company level is referred to the subsequent plan phase.
Other rules:
- Only the scope of ** is clearly stated in ** objective/description; conjecture or extrapolation of domain names/IPs not mentioned is strictly forbidden.
- The narrative description is based on what language to facilitate audit.
- ** Do not** call add_task_scope if there is no explicit asset scope in the target/description.
First use add_task_scope to register the scope (if any) and then call set_goals to submit the target.`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (background: target range/flag number/combat description
// etc). It is fed alongside the goal so the decomposer no longer splits blind — the prompt still
// forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// The target parsing is a one-time call: no transcript store attachment, so agentcore does not
	// attach session id to the ctx (it only attaches when there is a writer, see agentcore.Prompt).
	// And the gateway that prompts for cache/viscous routing using the session-id header (opencode
	// zen missing x-opencode-session directly 400 MissingSessionID) reads this value from the ctx —
	// no replenishment is "dialogue is ok, parse 400").
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints is always available (not dependent on the asset store): the body already
	// contains the "take action constraints first and then untarget" step (which can be reworded in
	// the agent edit page), so you just need to attach the tool here.
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "Target: \n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n \n task description (background information, possibly range of target/flag quantity/combat statement; for reference purposes only and not extrapolated): \n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// The 3 steps (pull constraints → register scope → untarget) require one tool call each, so
		// that the round is completed without missing set_goals before the end.
		MaxTurns:     8,
		NonStreaming: nonStreaming, // Provider.Complete when the profile is non-fluid
		MaxTokens:    maxTokens,    // 0 = no limit, determined by the server default value
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
