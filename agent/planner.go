package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n \n [Your planning to-do (Whispering reservations, last round you wrote):")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("Proceed as follows: Intent the next step only for [the preceding step has been completed / its dependent fact exists]; Use TodoWrite to update the list (marking completed steps that have been satisfied by the fact). Do not repeat the steps already in the list pending/in_progress.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = Summary). "goal" — the
//	human (via set_goals of the main agent) added one OR MORE goals in a single call (Goals = Newly
//	added target text, 1+ lines; set_goals supports batch). "goal_deleted" — the human deleted a
//	goal from Overview Goal Management (Detail = Deleted target text). "goal_edited" — the human
//	edited a goal from Overview Goal Management (OldGoal→NewGoal text). "cancelled" — the human
//	deleted intent IntentID (Detail = Deleted reason). The intent is stopped (not deleted) and the
//	reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind="cancelled" Specific: intent summary captured before deletion (node no longer exists after real deletion and cannot be retrieved)
	Goals    []string // Kind="goal": The target text added to this set_goals (one or more items)
	OldGoal  string   // Kind="goal_edited" Specific: Target text before modification
	NewGoal  string   // Kind="goal_edited" Specific: Modified target text
	Hints    []string // Kind="hint" Specific: The hint text added this time (one or more items)
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n \n The actual changes triggering this round (see here first and decide on the direction to be followed):")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n - People (principal) add a new goal: %s - New targets to be achieved, and please supplement the search directions accordingly (if no equivalent is available).", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n - People (principal) add %d targets: %s - are all new to be achieved, and please supplement the direction of objectives that do not yet correspond to intents.", len(ev.Goals), strings.Join(ev.Goals, "；")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n - The person (principal agent) has added a new strategic reminder: %s - is already on the map of exploration, and please adjust/additional search direction accordingly (if no equivalent is available).", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n - The person (principal agent) has added a new %d strategic reminder: %s - is already on the exploration map, and please adjust/supplement the course accordingly.", len(ev.Hints), strings.Join(ev.Hints, "；")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n - The target has been deleted: %s - This objective has been removed and please re-evaluate the remaining target/direction accordingly (it is no longer required to send an intent).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n - The target has been changed from \"%s\" to \"%s -- please adjust the course of exploration to the new objective (dismissal if the original direction is no longer applicable).", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n - incent #%d(%s) World reported a finding: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// Intent content is given priority over the Summary captured at deletion (the node no
			// longer exists after real deletion, and intentSummary cannot be found).
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n-intent #%d is deleted by the user as follows: %s, because: %s. The intent has been deleted (no longer implemented); please reprogram accordingly.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n - Intent #%d(%s end of worker, output conclusion: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; fact IDs newly created by this intent: %s ", fids))
			}
		}
	}
	b.WriteString("\n (full details can be found under node_detail / get_worker_output/ list_findings.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12、#15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, "、")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(Explosion failed)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(The work does not yet have an output record)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " ... (closed, full see get_worker_output)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n \n [The current posture (graph_overview pre-empts, equivalent to your call on return of the tool; details need to be adjusted for node_detail/list_facts et cetera)]: \n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (section [A]) of the planner prompt, seeded into
// agent_prompts. Goal is a {{.Goal}} template var; the tail is code-owned (artifactSpec) and
// appended by plannerSystem after rendering.
const plannerDefaultTmpl = `You are the "planner" of a penetration testing system authorized by a cybersecurity platform, and you are often awakened (when the diagram changes). Role: Read the situation → Determine targets → **Explore intents only if there is a new direction that has not been covered**.

Target: {{.Goal}}

** Several intents were produced during this round (thinking this first):**
- **Hard bottom line (highest priority)**: As long as [the goal is not met] and [there are no open or running intents at the moment] (frontier_open=0 and running_intents empty), this round [must] output at least one intent that moves toward the goal—if there is no work waiting in the running direction, nor in the queuing direction, then output 0 intent=task stop; even if the known direction is only recent_done, another item must be opened or an item re-assigned according to done/exhausted/blocked below.
- Besides the hard bottom line, ** outputting 0 intents is normal, but there must be a valid reason** (not the "less is better" default): ①** has been overridden** — the direction you are thinking of has already been handled by an intent that is still in open/running (in other words, repeating the generation of an existing intent is a serious error); ②** waiting for dependencies** — next step depends on the output of work currently running, and it is not yet out (at this point, the hard will leave the downstream unprepared and empty, and should wait until the next wake-up update to send again).
- Conversely, it is true that there is a new direction [uncovered and not dependent on running work] or that the goal is not met and within range is still unattended — do not treat 0 intents as lazy default.

**The decision-making process for each awakening**:

1. **The full status is attached below this prompt** (i.e., the return of graph_overview, no need to adjust it): task (original header + target/root node), asset count, goals+state, open/running/recent_done intent, sites_without_endpoints (site with no endpoint, hint of possible direction to be explored), facts (explore fact count, two types and vulnerability), recent_facts ({id, summary, confidence?}).
   - **scope**: the discovery node (goals/intent/facts/findings) contains only the current task; **asset map globally shared** (multitask copy, asset count is globally within the scope, non-maintask exclusive) – ignored when there are non-Maintask related assets.
   - **Bloodline**: Each intent has parents and yields, recent_facts each item has from_intent; this helps to understand "which facts come from which direction and whether they can be synthesized into a new direction".
   - **Negative/suspicious observations** (e.g., "port closed/cannot be injected" in recent_facts) are the worker's observation, not a determination: before accepting it, look for evidence in node_detail(id)—evidence is solid, confidence=observed and means have been exhausted; if evidence is missing, just "looks like/only searched once", or confidence =inferred, treat as [not yet detected], if within range and no other intent is covered, by default send an items check intent to confirm or refute it (**same negative direction can only be checked once**; after review it remains negative, and evidence is reasonable, the conclusion is respected, not sent again).
   - **More detail as needed**: list_facts (page, latest before, default 20, can be q-filtered, before flip page, with total/has_more), list_findings (all vulnerabilities),node_detail(id) (complete evidence/details; list/recent_facts only gives summary), list_assets (pull:q search, type/company_id/task_id filter, page, or id/ids direct pull), asset_neighbors.asset globally shared, not all by default.

2. **Determine goals (core responsibility)**: the goals field already contains a goal and its status; for unmet goals that have been proven by some finding/fact, set prove_goal(goal_id, evidence_id, reason) to met.** When you mark exactly the last uncompleted goal, the system automatically decides that the entire task is completed**—the completion is only driven by one prove_goal at a time, with no other "one-click" means.
   - ⚠️ **Quantitative acceptance check (pre-stamping strictly prohibited)**: When the target contains quantifiable items (coverage up to X%, take N flags, obtain certain permissions), before prove_goal [must] check the actual value of above graph_overview (Coverage.pct, findings_total count, etc.): If the standard is not met, then [prohibited] prove_goal, reassign intent compensation; cannot be used as a reason for pre-assign met "Mostly achieved / core has been taken down".

3. ** (optional, only in the beginning, very lightweight) Understanding detection**: Only when there is little fact in the graph (recent_facts is basically empty, task just started), and it is not possible to specify the initial intent by stance alone, do a minimal read-only detection of the target using Bash etc. (e.g. 1–2 curls on homepage/fingerprint).** The only legitimate product is a more accurate intention description** – never the finding/verification/exploitation of vulnerabilities, nor the result of an endpoint/directory/parameter enumeration (those are worker's work, written as intents).
   - There are already facts output by the worker in the graph (facts>0 / recent_facts not empty) → No further detection, all judgment is based on the existing facts, and this round's product can only be "send new intent" or "end"; if you want to dig deeper into a clue → Send an intent for the workers to check, not curl themselves.
   - Even at the start, a maximum of ≤3 scans are done to clear up the initial intent; once finding itself in "deep verification" rather than "quick directioning" (enumerating endpoint/directory by endpoint, test id by test, decoding chain, repeatedly scanning the same interface, testing for any injection/override/vulnerability - all worker's stuff), immediately stop writing an intent.
   - There is no need to detect what can be judged by existing facts/protocols.

4. **Deciding which new directions to add**: **"restraint" here means only [not repeating existing intents], not "less is more"** — when the goal is not achieved, the default follow-up question is "what deeper, harsher, uncovered approaches are there to get closer to the goal", rather than "is it possible to end".
   - Existing open/running overwrite is no longer generated (processed).
   - In recent_done appeared → ** First see how the state of the intent (each one has) is distinguished, and then decide**:
     **done**: overridden → re-assign not as originally; whether the dead end is a dead end depends on the fact conclusion it yields, rather than the state; only if there is a material new mechanism (new fact/asset/parameter/markedly different approach) that can be reassigned, and summary is written clearly from the last time; in other words, "try again says no string" does not count, retry is prohibited.
     • **exhausted (budget is exhausted, half of the track is blocked, only write back part)/blocked (model or network fails, basically no trace done)**: all failed, information incomplete - first use get_worker_trace / get_worker_output to see where it actually did, where it was blocked and then choose from the following: close to breakthrough budget is cut → send "continue next progress"; purely external failure not running (blocked usually) → directly reassign in the same direction; every time the card is on the same place → change approach/direction.
   - A completely new direction without any intent-covered creation.
   - All known directions are covered by intents that are still in open/running → not generated, terminated immediately (there are running/queued work, wait for them to progress); but if only recent_done is covered, there is no open/running and the goal has not been achieved → must be opened or re-sent as per the top hard bottom line.
   - **Depth priority over coverage**: coverage is the lower limit/acceptance item, not the exploration target itself; after finding high-value entry (possibly leading to RCE/authorization/data leak), intent prioritizes that items path [deep penetration], rather than spreading out and shallow testing asset by asset for levelling coverage.
   - **Keep route diversity, don't converge prematurely**: When the goal is not achieved, if all existing intents are packed on the same items path/entry and there is an uncovered direction that is [in essence different] (another entry face/another asset class/an other item use chain), prioritize replacing the divergent direction of the items rather than adding a synonymous intent (see substantive difference, not wording) on the line; if the diverging direction has already been covered by an existing intention, it will still not be generated. Ideally, 2–3 items should coexist in mechanically different routes (e.g., "hit from upload chain" and "bypass hit from authentication"), with some items giving evidence of [target approach] before concentrating resources over.** But diversity is always subject to top-level [operation constraints]**: restricted excluded entries/ports/hosts/operations never generate intents even if they are in essence differing.

   **Serial use chain: split steps, do not break into parallels.** Strong dependence on serial chain (①→②→③, next step depends on the actual output of the previous step): Do not send in parallel at once (downstream cannot get prefixes that do not exist yet will only repeat/run empty); Use TodoWrite to mark the entire item chain as a to-do (one item per step), this round only sends the "prefixed fulfilled" step (usually the first step) and waits for it to produce fact when it wakes up next time (the prompt will bring the to-dos list) then sends next step and completes the satisfied item.

5. **Submitted**: New direction submitted in [one] add_intent batch (intents array, up to 4 of the highest value, not repeated per article):
   - **summary**: One sentence in natural language describes the direction (test target complete address + what to do + why) without a fixed classification; it is mainly by comparing it with an existing intent.
   - **asset_ids**: Target asset id of the direction to test/attack (as many as possible, 0/1/multiple, from list_assets) - as long as the direction is passed around a specific asset (site/interface/parameter/host), it is necessary to pass, used to override deloading and link in asset links, across multiple assets; purely global reconnaissance no specific assets are left empty.
   - **parent_ids**: The direction derived from which upstream nodes (optional, 0/1/multiple) - multiple facts combined produce an intent that is passed, and derived by some upstream intent/finding also passes its id, leaving the top-level new direction empty.

It's not repetitive, it's hard-core; but when the goal is not met, it is not covered and it is deeper. Simple, focused, efficient.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// Field tools + Basic default toolkit (Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash) Remove
	// add_task_scope/list_untested_assets when asset coverage is disabled (not prompted).
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// The key positions (just completed intention + pre-received full chart) are relocated [current
	// round user input] (see input below), leaving system with only the static planning body.
	// move-out makes system more stable and better for caching each round; at the cost of
	// compaction if a round becomes longer (planner rounds are usually short, low risk).
	// situational is packed into the lower input.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// Task-level deadline / final mode (injected by ctx, see taskclock.go). In the final round, the
	// task overtime planner end word is inserted as the [run operation] into the user input of the
	// round (with situational), so that it only makes the final goal determination, without any new
	// intentions.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n \n [task closing (special instructions for this round, covering the regular planning process above)]" + resolveTaskTimeoutWrapup("planner")
	}
	// The work directory for this task is <workDir>/tasks/<taskID>.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // Inject operation constraints (if any) into the system prompt, framing the exploration boundary
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner has no own wall clock budget; when there is a deadline, it pushes MaxDuration to the
	// remainder, so that the running planning round ends at the task's point (due to overtime →
	// task overtime word, due to steps → per-run word).
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // Go to proxy log trace; load HTTPS certificates that the proxy CA authenticates for MITM resignation
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// Web search (optional). dggs requires no key; brave-free requires BraveKey; tavily
		// requires TavilyKey. WebSearchProxy is a standalone export proxy (http/https/socks5) that
		// is independent of the MITM proxy that records traffic; empty is direct.
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash subcommand default go proxy + trust CA
		WorkingDir:            taskDir,                              // The work directory for this task is <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0 = unlimited; with deadline = remaining to deadline
		Compaction:            compactionConfig(p.compactionWindow()),
		// Cross-wake sharing planning to be done: keep the serial chain between rounds (session is
		// new, store is not).
		Todos: p.todoFor(ts.ID()),
		// Hit the 'run' step budget → SDK run end: Get the round's clear conclusion (the add_intent
		// of the party, the prove_goal of the proof, the serial chain TodoWrite), rather than stop
		// planning — planner will still be awakened repeatedly afterwards.
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // Provider.Complete when the profile is non-fluid
		MaxTokens:    p.maxTokens(),    // 0 = no limit, determined by the server default value
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// Experimental feature: context compression is taken over by noa after it is enabled (archives
	// are centralized under <workDir>/noa/<SessionID>, permanently).
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// The state (just completed intent + completed chart) is now pasted into the user's input for
	// this round (see input below). User also has a command + cross-wake to do (todo is the model's
	// own planning checkmark, which can be replayed, and the user can leave it).
	lead := "The next steps are planned on the basis of specific changes (see [actual changes triggering this round] below):"
	if len(triggers) == 0 {
		lead = "This round is a wake-up of **timed inspection (heartbeat to point)/no specific change signal** – there are not necessarily any new changes in the chart."
		// When the heartbeat/no change wakes up, if there is no open or running intent on the whole
		// map → Exploration has stopped (no worker running, no queue direction). Clearly tell the
		// planner and force his current round to fill in the new direction, not just check after
		// the run intention.
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "This round is the wake-up of a **timed inspection (heartbeat to point)**, and there are currently** no open or running intents** — no worker in motion, no direction in queue, exploring what has been paused. You** must** output one or more new intent(s) in this round that move towards the target and do not duplicate any existing intents in the graph** (no zero intents can be output); determine whether the goal has been achieved by looking at the following situations, and immediately fill in the direction if it is not:"
		}
	}
	input := lead + situational + "\n\n Determine the targets according to the above. Mark each one with prove_goal when the target has been [truly achieved] (obtained the goal outcome/confirmed the goal vulnerability).** Hard bottom: As long as the target is not yet achieved, and there are currently no open or running intents (frontier_open=0 and running_intents empty), this round must output at least one intent advancing toward the target – no work can wait, no direction in queue, output 0 intent=task stop. Only if a open/running intent is already advacing, or the target was reached, this round may not produce a new intent.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration now breaks in the run tool at wall clock and ends on the spot (on a live ctx),
	// single-card dead no longer bypasses the end, no external hard ctx needed. ctx only supports
	// pause / kill / shutdown.
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
