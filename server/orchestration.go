package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// jsonResult marshals v to a JSON tool result.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// This document implements P2's "cross-task scheduling toolset" (docs/run scheduling §2 P2). These
// are host tools - they need access to Manager (store for any task), Engine (pause), and build task
// processes, so they live on the server layer. Read classes redirect existing per-task tools to run
// on the store of the target task (build a temporary ToolSet and Call its corresponding tool) using
// exactly the same logic; control classes (spawn/pause) directly tune Manager/Engine. They seed
// into the tools table like flow classes, and bind by agent (only bind to the scheduling agent).

// hostTools is the runtime host-tool provider fed to ToolAugment: traffic tools
// (gated by capture) + cross-task orchestration tools + user-defined custom tools.
// The second return is the names of custom tools flagged `deferred` (schema
// withheld, routed via SearchExtraTools/ExecuteExtraTool). Per-agent binding still
// decides who actually sees any of them.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.platformTools()...) // Platform operating tool (build skill/tool/MCP, for Auto)
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] Loading failed: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred custom tools → name -> its bound agent keys. ToolAugment turns a
	// name into a deferred entry only for agents it's actually bound to (so we don't
	// advertise a tool the per-agent binding will drop from the callable set).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools returns the cross-task tool set. Bound per-agent via the
// tools table (default: no binding — opt-in for orchestration agents).
func (s *Server) orchestrationTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
		s.toolGetFindingTraffic(),
		s.toolBindFindingTraffic(),
	}
}

// --- schema helpers ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID reads an LLM profile id from a tool arg that may arrive as a JSON
// number (5) or a numeric string ("5"); returns 0 when absent/unparseable.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask resolves the `task_id` in the input, builds a ToolSet bound to
// that task's store, strips task_id, and calls the chosen per-task tool — so the
// cross-task read reuses the exact in-task logic against another task.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id It's mandatory."), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("tab does not exist: " + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify)         // General awakening (no dedicated callback writes it; reads no-op)
	tsx.SetNotifyHint(t.NotifyHint) // add_hint → Notes a "person added N new strategy hints:..." trigger and wakes up the planner
	return pick(tsx).Call(ctx, inner, nil)
}

// --- tools ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"Lists all tasks (id/description/target/state/runtime/parent task/LLM configuration) and orders the agents to use them for global control, seeing which task cards are overdue and which ones are being used LLM. Runtime: Running=Create→Now, End state=Creates→Last activity (seconds).llm_profile: Configuration name for task planner/worker, (activating configuration): Follows global activation.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.PG().LastActivityAll()
			// id -> name to resolve each task's pinned LLM profile.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "(Inactive configuration)"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("# %d (deleted)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles lists the available LLM profiles (name/model/active) so an
// orchestration agent can pick one for spawn_task's llm_profile. Never leaks keys.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"Lists available LLM profiles: id, name, model, format, and whether they are currently active. Use the llm_profile_id parameter for spawn_task with id to specify subtask exclusive to LLM (e.g. cheap model for reconnaissance, powerful model for exploitation).",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"Creates a new subtask and starts the discovery engine, returning task_id. Used to assign an event (such as a task/goal) as an independent task. parent_ref Optional: Enter the parent task id of the current scheduled association for the parent-child association.",
		objSchema(map[string]any{
			"description":            strParam("Description of task (short title)"),
			"goal":                   strParam("Task's target."),
			"parent_ref":             strParam("Optional: fathertask id (parental association)"),
			"source_task_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("Optional: Read-only inheritance of a list of source task ids (up to %d). Subtasks can only read references to the asset/conclusion identified by these tasks as a starting point; unlike pure parent-child pointers for parent_ref, this is content inherits.", db.MaxTaskSourceCount)},
			"llm_profile_id":         map[string]any{"type": "integer", "description": "Options: specify LLM configuration id for book task planner/worker; leave the space to take over parent task and return global active configuration"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "Option: task classtimeout (seconds). After the point, trigger a graceful close and enter the timeout end; leave empty or 0 = open-ended"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "Optional: planner Heartbeat trigger interval (seconds). From the last round of planning end/task start this value is reached and there is no trigger → Triggers a round of scheduling (deadlock + wake up to supervise worker in flight). Leave empty or 0 = default 600(10min);"},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "Optional: for simple tasks, an items seed intent (content = description + goal) can be sent directly at the time of creation to allow worker to run test without waiting for the first round planner; default false (go standard first plan then execute)."},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description          string          `json:"description"`
				Goal                 string          `json:"goal"`
				ParentRef            string          `json:"parent_ref"`
				SourceTaskIDs        []string        `json:"source_task_ids"`
				LLMProfileID         json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds       int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent      bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "unnamed task"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("Goal, fill it in."), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// Read-only inheritance source tasks: number cap + valid/unloaded/existing per id,
			// validation rules consistent with HTTP build tasks.
			if len(a.SourceTaskIDs) > db.MaxTaskSourceCount {
				return actool.Errorf(fmt.Sprintf("Association task selection %d", db.MaxTaskSourceCount)), nil
			}
			sourceIDs := make([]int64, 0, len(a.SourceTaskIDs))
			seenSources := map[int64]bool{}
			for _, raw := range a.SourceTaskIDs {
				id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || id <= 0 || seenSources[id] {
					return actool.Errorf("Associated task id invalid or duplicate"), nil
				}
				if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
					return actool.Errorf(fmt.Sprintf("Association task #%d does not exist", id)), nil
				}
				seenSources[id] = true
				sourceIDs = append(sourceIDs, id)
			}
			// LLM profile resolution: explicit id > inherit parent's pin > active(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM Configuration #%d does not exist or is not set API Key", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			var llmIDs []int64
			if pin != nil {
				llmIDs = []int64{*pin}
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				SourceTaskIDs:        sourceIDs,
				LLMProfileIDs:        llmIDs,
				TimeoutSeconds:       a.TimeoutSeconds,
				PlanHeartbeatSeconds: a.PlanHeartbeatSeconds,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.PG().SetParentRef(id, a.ParentRef)
				}
			}
			// Shared post-build process, duplicating the same section as HTTP build task (server.go
			// createTask) launchTask: seed + backend visiblely decomposes goals (round 0/LLM
			// steps/line by line goals) + engine.Run. seed_first_intent default false (standard
			// plan-first-run); simple tasks can be started directly with a work test.
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "Suspends the task (stop its planner/worker loop).",
		objSchema(map[string]any{"task_id": strParam("tab id to pause")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("tab does not exist: " + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "Reads an overview of the exploration map of the specified task (with graph_overview:set count/frontier/finding/ overlay, etc.), with task_id assigned to task.",
		objSchema(map[string]any{"task_id": strParam("pic.twitter. cit.")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "Reads the confirmation vulnerability of a specified task (including flag/PoC; each item has id/task_id/intent_id/vulnclass/severity/summary/status), and specifies the task with task_id.",
		objSchema(map[string]any{"task_id": strParam("pic.twitter. cit.")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "Injects a strategic hint to the specified task (which will be read when the task's next round is generated). \n"+
		"Priority batch: multi-items hints are submitted in the hints array at once (returns ids array, equal to hints, failed item id=0); single items omit hints directly to top-level text.",
		objSchema(map[string]any{
			"task_id":      strParam("pic.twitter. cit."),
			"hints":        map[string]any{"type": "array", "description": "[Prefer this] suggests an array with each element field at the same top level (text/asset_ids/traffic_refs).", "items": objSchema(map[string]any{"text": strParam("Hint Contents"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()})},
			"text":         strParam("[single] hint"),
			"traffic_refs": agent.HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "anchor set id (optional, 0/1/ multiple;set id in task)"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"Look at the execution of a work(intent) in the specified task: get_task_worker_trace (task_id, intent_id) See the summary of steps; then use step_ids=[...] to get the complete contents of those steps (up to 5 at a time, only returning the first 5).",
		objSchema(map[string]any{
			"task_id":   strParam("pic.twitter. cit."),
			"intent_id": map[string]any{"type": "integer", "description": "intent id (work in task)"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Optional: Step id to complete (up to 5 at a time, with more than 5 returned before, the rest listed in omitted_step_ids)"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "Lists which work (int) + the number of steps that are specified for use in task and which work is worth looking at (reuse get_task_worker_trace).",
		objSchema(map[string]any{"task_id": strParam("pic.twitter. cit.")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "Search all work executions in the specified task by keyword (returns hit step summary + intent_id).",
		objSchema(map[string]any{"task_id": strParam("pic.twitter. cit."), "q": strParam("Search for keywords")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"Reads the full content of a discovery map node in a specified task (finding/fact/intent/target: summary + details/evidence/PoC).id is the id of the exploration node (such as report_finding returns, or the id in list_task_findings).Use it to get the complete evidence of the vulnerability before writing a vulnerabilities report.",
		objSchema(map[string]any{
			"task_id": strParam("pic.twitter. cit."),
			"id":      map[string]any{"type": "integer", "description": "Explore Diagram Node id (nonasset id)"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport writes/overwrites a finding's detailed Markdown report.
// finding_id is the id report_finding returned ("finding recorded: <id>", the
// finding node id). The write (SetFindingReportByNodeID) is keyed by node_id and
// task-agnostic, so this host tool needs no task_id / exploration store.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"Write/update the [Detailed Report] (Markdown full text, entire paragraph overrides old content) for a registered vulnerability. finding_id Passes the id returned by report_finding (the number in \"finding recorded: <id> \").The report recommendation includes: Vulnerability overview, impact and hazard, replication steps, evidence/PoC, fix recommendations.",
		objSchema(map[string]any{
			"finding_id":       map[string]any{"type": "integer", "description": "Target vulnerabilty id(report_finding returns id)"},
			"report":           strParam("Full full report, Markdown format"),
			"evidence_version": map[string]any{"type": "integer", "description": "get_finding_traffic Evidence version returned; used to prevent the report from overwriting new evidence changes"},
		}, "finding_id", "report"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				EvidenceVersion *int64          `json:"evidence_version"`
				FindingID       json.RawMessage `json:"finding_id"`
				Report          string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // Reuse of "number or number string" parsing
			if nodeID <= 0 {
				return actool.Errorf("finding_id Invalid"), nil
			}
			n, err := s.m.pg.SetFindingReportVersionByNodeID(ctx, nodeID, a.Report, a.EvidenceVersion)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("Vulnerability record not found for finding_id=%d (register with report_finding first)", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus mirrors listTasks' status derivation for the list_tasks tool.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds seeds the cross-task tools into the tools table so they
// are bindable per-agent (default: bound to nobody — opt-in for orchestration
// agents). First-insert only, like the traffic seeds.
func (s *Server) seedOrchestrationTools() {
	// task-op + platform tools default-bind to the built-in Auto agent (it is natively used to
	// operate the platform). The SeedTool first insert takes effect; the old library has been
	// replaced by the seedAutoDefaultBindings line in seed.
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings := autoAgents
		if t.Name() == "bind_finding_traffic" {
			bindings = json.RawMessage(`["reporter"]`)
		}
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedWorkerReadToolsUnbind() // list_facts/list_companies/list_worker_traces Unbind from worker default (one-time)
	s.seedWorkerReadbackRebind()  // Fix old migration error: replace search_all_worker_traces/get_worker_trace/node_detail with worker (once)
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()             // goals prompt Add "pull constraint" step → Add a new default version of the old library (one-time)
	s.reseedMainAgentPrompt()         // mainagent prompt added "Add_intent Asks If Target Is Created After Target Is Achieved" (one-time)
	s.reseedPlannerPrompt()           // planner Hint: Rewrite "0 intent" justification + incremental acceptance check (one-time)
	s.reseedWorkerPrompt()            // worker Hint: Plus negative conclusion proof threshold (one-time)
	s.seedReporterAgent()             // Preset "Report Writing" agent + tool binding + finding trigger (one-time)
	s.upgradeReporterTriggerMessage() // 1.1.1.1.1.1.1.1.1.1.1.1.1.1.1.1.1.1.1.1.1.1.1.1.1.1.1.1.1.1
	s.seedFindingTrafficTools()       // Added optional evidence parameters and read-only evidence tools, retaining user configuration
	s.seedFindingWorkflowTools()
	// Note: The default tool bindings for pentest do not require migration — BuiltinToolSeeds seeds
	// list_assets/insert_assets/report_finding/list_findings/list_companies with pentest at the
	// fresh initialization (the project does not have an old library, so no migration).
}

// refreshBuiltinToolSchemas propagates code schema/description changes on the
// orchestration + platform tools into already-seeded rows ONCE per version flag —
// SeedTool is first-insert-only, so a new param (e.g. spawn_task's llm_profile) never reaches an
// old DB otherwise. Preserves each tool's agent binding + enabled flag. Bump the flag whenever
// these tools' schemas/descriptions change in code.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v7_list_facts_paging"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// And make some of the built-in agent tools code defaults:
	// - goal_met: the old seed library description was misleadingly labeled "end this round of
	// planning", which would make the planner mistake it as a means to "end the empty round" and
	// misjudge the entire task as completed at the start.
	// - insert_assets: new related entry (marking whether assets are relevant to the current task,
	// deciding whether to include coverage), SeedTool first inserts only, the old library has
	// already seed schema otherwise does not receive this new parameter.
	// - list_facts: changed to a subpage, new limit/before/q entry added; empty schema of the old
	// library that has been seeded otherwise it will show "no parameters" in the tool management
	// page and the model will not get the parameter description.
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Printf("[tools] Refreshed orchestration/platform tool schema to codedefault (one-time)")
}

// unbindGoalMetDefault removes goal_met's default "planner" binding ONCE (guarded by
// a settings flag), so existing DBs match the new default of NO agent. goal_met bypasses
// per-goal prove_goal to declare the whole task done — powerful/risky and redundant with
// the prove_goal→auto-complete path — so it ships unbound; users can re-bind it per agent
// in the UI. A user's own binding to another agent is untouched (we only strip planner).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] goal_met Untie planner failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt prints the goals parser prompt to [current code default] — because the default
// body adds a new step called "set_constraints before unsetting goals", and SeedPromptIfEmpty
// inserts only first, the old library version 1 does not receive this step. Here, use version
// control [add a new version] and ResetPromptToDefault, the old version is still in history, and
// users can get it back from the version log if they have customized it. settings flag guard → do
// only once; after that, the default is bump this flag.
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Try it once, whether you succeed or not.
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // If the new library has not yet created an agent row, seedPrompts will use the latest seed default without this migration.
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// New library seedPrompts have been seed latest default → current version is now the code
	// default, no need to add duplicate versions.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] Goals hint redoing to new default failed: %v", err)
		return
	}
	log.Printf("[prompts] goals prompt added new default version (added pull constraint, one-time)")
}

// reseedMainAgentPrompt prints the mainagent prompt to [current code default] — the default body
// has a new instruction, "When all goals are reached, add_intent will ask if you want to register
// as a formal goal" and SeedPromptIfEmpty inserts only, so that the old library does not receive
// the existing version. Use version control to [add a new version] and ResetPromptToDefault, so
// that the old version is still in the history and can be retrieved from the version log if the
// user has customized it. settings flag guard → Do only once. No need to process the entire new
// library (SeedPromptIfEmpty is now the seed latest default).
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Try it once, whether you succeed or not.
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // If the new library has not yet created an agent row, seedPrompts will use the latest seed default without this migration.
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// New library seedPrompts have been seed latest default → current version is now the code
	// default, no need to add duplicate versions.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] hint redoing to new default failed: %v", err)
		return
	}
	log.Printf("[prompts] mainagent prompt added new default version (one-time request to build target after adding goal)")
}

// reseedPlannerPrompt prints the planner prompt to [current code default] — the default body is
// streamlined and rebuilt, and "restraint" is downgraded to "only repeat", and "depth priority over
// coverage" and "hard bottom line: goal not achieved and no running intent must be output" are
// added, and a bound is added to negative conclusion review. Each time there is a substantial
// change in the default, the flag below bump (current v2) will reload the stock of the old library.
// SeedPromptIfEmpty inserts only the first version, so the old library has already been reverted,
// so use versioning to add a new version and cut it over (ResetPromptToDefault), the old version
// remains in the history, and users can get back from the version log if they have customized it.
// settings flag guard → do only once. No need to process the new library (SeedPromptIfEmpty is now
// the seed latest default).
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Try it once, whether you succeed or not.
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // If the new library has not yet created an agent row, seedPrompts will use the latest seed default without this migration.
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// New library seedPrompts have been seed latest default → current version is now the code
	// default, no need to add duplicate versions.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] planner hint repainting to new default failed: %v", err)
		return
	}
	log.Printf("[prompts] planner hint has been added to the new version of default (Simplification + Restraint down + Deep Priority + Negative Review Top, one-time)")
}

// reseedWorkerPrompt prints the worker prompt as [current code default] — the default record_fact
// paragraph removes the whole sentence "negative class conclusion write observation + tempting
// reading", and decouples confidence (observed/inferred) from "whether exhausting means of intent"
// (which can mislead planners), while tightening facts arrays to a very few exceptions that are
// "completely independent of each other and cannot be merged". bump flag to v3 Reprints the stock
// of the old library. SeedPromptIfEmpty insert only, the old library has already been reverted, so
// use versioning [add a new version] and cut over, the old version is still in the history.
// settings flag guard → Do it once. No handling needed. Fully identical to reseedGoalsPrompt.
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v4"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Try it once, whether you succeed or not.
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // If the new library has not yet created an agent row, seedPrompts will use the latest seed default without this migration.
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// New library seedPrompts have been seed latest default → current version is now the code
	// default, no need to add duplicate versions.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] worker tipback to new default failed: %v", err)
		return
	}
	log.Printf("[prompts] worker prompt added new default version (check context converges to list_assets/list_findings, remove list_facts/node_detail/asset_neighbors, one-time)")
}

// The report must be written without the reporterToolCallMessage being asked to read
// get_finding_traffic first. The tool is read-only, "not dependent on the capture switch", and can
// read manually bound evidence whether or not the automatic binding is turned on. If you write here
// "Enable auto-binding only for reading", then by default with the reporter off, no
// evidence_version is passed, SetFindingReportVersionByNodeID is written as legacy semantics -1,
// and the vulnerability details are exported with Markdown.
const reporterToolCallMessage = "report_finding has just registered a vulnerability above. Please read the finding_id (independent vulnerabilities record ID) and finding_node_id (exploration node ID) returned by JSON," +
	"get_finding_traffic(finding_id) to read the current list of events and its version (empty list is normal, reporting as usual);" +
	"If the running guidelines enable automatic binding, verify and associate this vulnerability flow before reading it. Use node details finding_node_id." +
	"Finally call update_finding_report (finding_id=finding_node_id, report, evidence_version= actual read version) to save," +
	"evidence_version Must be transmitted, otherwise the report will be permanently marked as pending update. Do not mix the two numbers."

// The old version triggered messages (0.3.8 and earlier). Only records that remain exactly the same
// as it is will be migrated, and changes made by users will remain unchanged.
const reporterToolCallMessageV1 = "There is just one vulnerability registered above report_finding. Please remove finding_id from the trigger context" +
	"(tool returns \" finding reported: <id> \") and task id, writing the detailed report on vulnerabiliti according to your duties," +
	"Finally, call update_finding_report(finding_id, report) to save."

// upgradeReporterTriggerMessage Refreshes the reporter trigger message that is still the default
// text in the old library to a new version. seedReporterAgent is guarded by reporter_agent_seed_v1
// and writes the trigger only when a new agent is created, so the upgraded library does not get the
// new text — tool schema The evidence_version is filled by seedFindingTrafficTools, but there is
// nothing else to tell the reporter to use it.
func (s *Server) upgradeReporterTriggerMessage() {
	const flag = "reporter_trigger_evidence_version_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Try it once.
	triggers, err := s.m.pg.ListTriggersFor("reporter")
	if err != nil {
		log.Printf("[reporter] Reading trigger failed: %v", err)
		return
	}
	for _, t := range triggers {
		if !t.OnToolCall || t.ToolCallMessage != reporterToolCallMessageV1 {
			continue // The user changes or does not find the trigger, and it remains unchanged.
		}
		t.ToolCallMessage = reporterToolCallMessage
		if err := s.m.pg.UpdateTrigger(t); err != nil {
			log.Printf("[reporter] Upgrade trigger failed: %v", err)
			return
		}
		log.Printf("[reporter] Trigger message upgraded to read and return evidence_version")
	}
}

// seedReporterAgent Presets a custom agent for report writing (builtin=false, can be edited/deleted
// in the UI): binds update_finding_report + task query tool and attaches a trigger called
// "report_finding is triggered when called" - it is called to write a detailed report every time a
// vulnerability is registered. one-time (settings flag guard): the user deletes it and does not
// rebuild it. dependencies: the orchestration tool is already in the SeedTool library above this
// function, so it can be bound.
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // Try it once, whether you succeed or not.

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // key is occupied (built by the user) - not overridden
	}
	a, err := s.m.pg.CreateAgent("reporter", "Report writing",
		"Vulnerability: Automatically triggered at FindingVulnerality, retrieves the report Markdown following the process of implementation.")
	if err != nil {
		log.Printf("[reporter] Creating agent failed: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] Seed program failed: %v", err)
	}
	// Trigger run policies: parallel + none - a vulnerability report, multiple findings written
	// simultaneously. merge must be none: otherwise (by default all) a wave of findings will be
	// merged into one run, and parallelization is meaningless. maxParallel=5: up to 5 report
	// sessions at the same time, to avoid too many LLM calls in a single run.
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] Setup to trigger running policy failed: %v", err)
	}
	// The tools it requires to bind: writing report + reading evidence/implementation
	// process/attitude.
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] Binding tool failed: %v", err)
	}
	// Triggers: report_finding is triggered when called (the tool returns "finding recorded: <id>"
	// with finding_id, the task id is also in the trigger message).
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:        "reporter",
		Enabled:         true,
		OnToolCall:      true,
		ToolNames:       []string{"report_finding"},
		ToolCallMessage: reporterToolCallMessage,
	}); err != nil {
		log.Printf("[reporter] Failed to create trigger: %v", err)
	}
	log.Printf("[reporter] Prearranged \"Report writing\" agent + finding trigger")
}

// seedAutoReportFindingBinding adds "auto" to report_finding's binding ONCE so
// conversation-context agents can call it without requiring an intent_id.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] report_finding Desfault binding failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings adds "planner" to report_finding's binding ONCE
// (guarded by a settings flag), so existing DBs — whose report_finding row was
// seeded as worker-only — also let the planner record findings. Fresh DBs already
// get it via PlannerTools(); this only backfills without overriding a user unbind.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] report_finding Failed to bind: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding adds "planner" to list_assets's binding ONCE
// (guarded by a settings flag), so existing DBs — whose list_assets row was seeded
// as auto/pentest-only — also let the planner query the asset store by DSL. Fresh
// DBs already get it via PlannerTools(); this only backfills without overriding a
// user unbind.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] list_assets Failed to bind: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind changes add_company_scope's default binding ONCE on
// existing DBs (guarded by a settings flag): the tool moves off worker and onto
// planner — defining a company's asset scope is a planning/main/auto concern, not
// something a worker does mid-exploration. Fresh DBs already get planner via
// PlannerTools() and lack worker via WorkerTools(); this only backfills old rows.
// One-shot + flag-guarded so a user who later re-binds worker isn't overridden.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // Worker → Planner default binding switch
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] add_company_scope Failed to bind: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[Worker] add_company_scope Untie failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadToolsUnbind strips the read-context tools off worker's default
// binding ONCE on existing DBs (guarded by a settings flag): a worker executes one
// intent and writes back — reading facts/companies and listing all workers' traces is
// a planning/main concern, not the executor's. Fresh DBs already lack these via
// WorkerTools(); this only backfills old rows without overriding a user who
// deliberately re-binds worker. Each RemoveAgentFromTool is per-tool +
// membership-guarded, so planner/mainagent bindings of the same tool are untouched.
//
// NOTE: search_all_worker_traces / get_worker_trace / node_detail are intentionally NOT
// unbound — worker owns them for cross-work look-back + node drill-down (see WorkerTools).
// They used to be in this list back when worker lacked them; seedWorkerReadbackRebind
// repairs DBs whose old run stripped them.
func (s *Server) seedWorkerReadToolsUnbind() {
	const flag = "worker_readtools_unbind_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, k := range []string{
		"list_facts", "list_companies", "list_worker_traces",
	} {
		if err := s.m.pg.RemoveAgentFromTool("worker", k); err != nil {
			log.Printf("[worker] %s Failed to untie from worker: %v", k, err)
			return // If the error is not flagged, retry next time.
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadbackRebind re-binds the cross-work look-back / drill-down tools onto
// worker ONCE (guarded by a settings flag): an earlier seedWorkerReadToolsUnbind wrongly
// stripped search_all_worker_traces / get_worker_trace / node_detail from worker after
// they had been added to WorkerTools(), so any DB that ran that migration lost them.
// Fresh DBs already have them via WorkerTools() and this is a harmless no-op there.
// One-shot + flag-guarded so a user who later deliberately unbinds them isn't overridden.
func (s *Server) seedWorkerReadbackRebind() {
	const flag = "worker_readback_rebind_v2" // v2: Add node_detail
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("worker", []string{
		"search_all_worker_traces", "get_worker_trace", "node_detail",
	}); err != nil {
		log.Printf("[Worker] Looking back/detailing tool completion failed: %v", err)
		return // If the error is not flagged, retry next time.
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings adds "auto" to the task-op + platform tools' bindings
// ONCE (guarded by a settings flag), so existing DBs whose tool rows were seeded
// before Auto existed still give Auto its default toolset — without re-adding it
// after a user deliberately unbinds.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3: replace old asset tool name with insert_assets/add_company_scope
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// Asset tools: Auto operating platform frequently see / register assets, manage company scope.
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] Default binding failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
