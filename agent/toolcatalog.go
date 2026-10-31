package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// This document turns the built-in tools from pure code to an enumerable directory that can be
// overridden by the DB:
// - BuiltinToolSeeds(): Extends the built-in toolset of the three executing agents into a seed
// record (key + description + parameter schema + default agent binding) for the server-side startup
// to seed in the tools table.
// - ToolResolve hook: on runtime, the tools line in the DB does "filter by agent + override
// description/schema + inject parameter defaults" for the assembled tool. Key/handler is still in
// the code layer, and the DB only changes "essay and defaults".

// ToolSeed is a seedable snapshot of the built-in tool: key is CoreTool.Name() (bound to the
// handler, UI read only), Desc/Schema is the tool definition from the code, and Agents is the agent
// the code assigns it by default.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), primary key, unchangeable
	Desc   string         // Top-level description (can be overridden in the UI)
	Schema map[string]any // Parameters JSON-Schema (structure read-only, description/default can be changed in the UI)
	Agents []string       // The default agent key (worker/planner/mainagent)
}

// builtinToolsByAgent constructs the domain toolset for each executing agent with a "read-only"
// empty shell ToolSet (nil stores).
//
// Intentionally excluded: SDK general tools actool.DefaultTools()
// (Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash): They are fixed ownership for each agent, no "to
// whom" trade-offs, and most of the descriptions are in Prompt() (this table only covers
// Description(), causing half-override misguided).
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals binds set_goals + set_constraints by default: they write the unpacked goals and the
		// extracted operation constraints to the library.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto is the default binding vulnerability report + asset management tool, other domain
		// tools can be checked in the UI as needed. New libraries are written by this seed; old
		// libraries are migrated by seedAutoDefaultBindings.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// The default binding for pentest (independent penetration agent): check assets / insert
		// assets / report vulnerabilities / check vulnerabilities / check enterprise. New libraries
		// are written by this seed; old libraries are migrated by seedPentestDefaultBindings.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound: These system tools are placed in the usual directory (visible on the web, can be
// checked manually by agent), but by default [does not bind any agent] - ToolResolve empty bindings
// are discarded for all agents, must be explicitly opted-in. The reason why they remain in an
// agent's base toolkit (such as goal_met in PlannerTools): firstly, so that seed can construct it
// to get desc/schema, and secondly, so that when the user manually binds back it runs out of the
// base, ToolResolve stays there.
//
// goal_met: bypass the prove_goal by prove_goal, directly from the global announcement of the
// completed task, is important and risks misjudging, and repeats with "prove_goal mark the last
// goal → automatic termination", so by default no agent, manually bound when needed.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds remerges the built-in tool sets of each agent into a seed list: tools with the
// same name (such as list_assets that have multiple agents) are combined into one row, and the
// Agents merge set; tools in defaultUnbound are forced to be empty.
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // In directory, can be bound manually, but by default no agent (store [] instead of null, consistent with other tools)
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// Tools with no matching DB row (MCP/skill/host tools like traffic) pass through untouched. nil =
// tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
