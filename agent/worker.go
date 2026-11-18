package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Worker is an LLM work agent (docs §4.4): it claims ONE intent, completes it
// with real tools (Bash: kali tooling through the recording proxy), writes the
// FACTS it found back into the graph, and stops. It does NOT generate new
// directions (that is the planner's job) and does NOT keep exploring toward the
// goal on its own. Multiple workers run concurrently as goroutines.
// WebSearchOpts is the web-search backend selection the server pushes into each
// agent (planner/worker/main). Enabled=false leaves the web_search tool off.
// Backend is "ddgs" (no key), "brave-free" (BraveKey required), "tavily"
// (TavilyKey required), or "deepseek" (DeepSeek* required, filled from the
// active LLM profile). It maps directly onto agentcore.Options.
// Proxy is a dedicated egress proxy for the search request (http/https/socks5),
// independent of the traffic-recording MITM proxy — set it when the search endpoint
// is only reachable via a VPN/SOCKS proxy. Empty = direct.
//
// Note that the deepseek backend is different from the other three: DeepSeek does not have a search
// interface that can be called directly, searches exist only inside its Anthropic-compatible
// messages interface (web_search_20250305 server tool), so each search consumes one model call, and
// the search request is sent by the DeepSeek service side—no native proxy, no inbound traffic
// trace.
type WebSearchOpts struct {
	Enabled   bool
	Backend   string
	BraveKey  string
	TavilyKey string
	Proxy     string
	// DeepSeek* comes from the currently activated LLM configuration (only anthropic formatted
	// DeepSeek official endpoint), not a standalone configuration, and changes with LLM
	// configuration switches.
	DeepSeekBaseURL string
	DeepSeekAPIKey  string
	DeepSeekModel   string
}

type Worker struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	workDir         string
	proxyAddr       string
	proxyCACert     string            // recording proxy's CA cert path (for WebFetch HTTPS verify)
	webSearch       WebSearchOpts     // web_search tool backend selection (off by default)
	tx              *transcript.Store // raw LLM conversation persistence (nil = off)
	window          int               // context window in tokens (for compaction)
	windowFn        func() int        // optional dynamic task-chain minimum
	maxTurns        int               // max agent turns per run (0 = unlimited)
	// runTimeout is the wall-clock budget for the main exploration of one intent
	// (0 = unlimited). When it fires, the run is cut and a settlement round is
	// forced so already-identified facts get written back instead of being lost.
	runTimeout time.Duration
	// extraTools are host-provided tools (e.g. traffic query, oast) appended to
	// the worker's graph write-back tools.
	extraTools []actool.CoreTool
	// injectConstraints resolves whether this task's operation constraints get
	// injected into the worker system prompt. Read per run so the settings toggle
	// takes effect without rebuilding the agent. nil = inject (default).
	injectConstraints func() bool
	// nonStreamingFn resolves whether this run uses the non-streaming (Complete)
	// path. Read per run so a profile/task toggle takes effect without rebuilding
	// the agent. nil = streaming (default).
	nonStreamingFn func() bool
	// noaEnabledFn resolves whether this run uses the experimental noa context-
	// compression mechanism. Read per run, like nonStreaming. nil = off (built-in
	// compaction).
	noaEnabledFn func() bool
	// maxTokensFn resolves the per-reply output cap in tokens, on the same
	// per-run basis. nil or 0 = send no cap and let the endpoint decide.
	maxTokensFn func() int
}

// WorkerSessionID returns the stable transcript key used by a worker intent.
// Worker slots are reusable, so the intent id (rather than work#N) is the
// session identity. Keep this helper public so the Worker message API and UI
// can refer to exactly the conversation that will be resumed.
func WorkerSessionID(explorationID, intentID int64) string {
	return fmt.Sprintf("exp%d-worker-i%d", explorationID, intentID)
}

const workerChatMarkerPrefix = "<!-- ARTEX_WORKER_CHAT:"

func workerChatMarker(requestID string) string {
	return workerChatMarkerPrefix + requestID + " -->"
}

func hasWorkerChatMessage(messages []llm.Message, requestID string) bool {
	marker := workerChatMarker(requestID)
	for _, message := range messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), marker) {
			return true
		}
	}
	return false
}

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default). Read per
// run so a profile or task-chain toggle takes effect without rebuilding.
func (w *Worker) SetNonStreaming(fn func() bool) { w.nonStreamingFn = fn }

func (w *Worker) nonStreaming() bool { return w.nonStreamingFn != nil && w.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (w *Worker) SetNoaEnabled(fn func() bool) { w.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (w *Worker) SetMaxTokens(fn func() int) { w.maxTokensFn = fn }

func (w *Worker) maxTokens() int {
	if w.maxTokensFn == nil {
		return 0
	}
	return w.maxTokensFn()
}

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the worker system prompt. nil = inject (default).
func (w *Worker) SetConstraintInject(fn func() bool) { w.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (w *Worker) wantConstraints() bool { return w.injectConstraints == nil || w.injectConstraints() }

// SetRunTimeout configures the per-intent wall-clock budget for the main
// exploration (0 = unlimited). When it fires, the SDK settlement phase still runs
// so facts are never lost to a timeout. Safe to call before Execute.
func (w *Worker) SetRunTimeout(run time.Duration) {
	w.runTimeout = run
}

// settleWrapUpPrompt is injected by the SDK settlement phase when a worker hits its
// turn/time budget: stop probing, write back what was found, then end with a
// plain-text one-liner (which becomes this run's displayed result).
const settleWrapUpPrompt = "Do not run any more commands/detections. Please: (1) write back items by item of what you have identified above but haven't written back -- new asset insert_assets, discovery conclusion/fact record_fact, confirmation vulnerability report_finding; and (2) **finally, in a single sentence of plain text** summarize what you did and what your key conclusions were (this sentence will be displayed as the result of this run, and must be output)."

func NewWorker(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int, extra ...actool.CoreTool) *Worker {
	return &Worker{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, extraTools: extra}
}

// defaultToolsExcept returns actool.DefaultTools() minus the named tools (by
// CoreTool.Name()). Used to trim SDK default tools an agent shouldn't have.
func defaultToolsExcept(exclude ...string) []actool.CoreTool {
	drop := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		drop[n] = true
	}
	all := actool.DefaultTools()
	out := make([]actool.CoreTool, 0, len(all))
	for _, t := range all {
		if !drop[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

func (w *Worker) SetCompactionWindowResolver(fn func() int) { w.windowFn = fn }

func (w *Worker) compactionWindow() int {
	if w.windowFn != nil {
		return w.windowFn()
	}
	return w.window
}

// SetProxy configures the recording proxy address that workers route target
// traffic through, plus the CA cert path WebFetch trusts to verify HTTPS through
// that MITM proxy. Empty addr disables the hint.
func (w *Worker) SetProxy(addr, caCert string) { w.proxyAddr, w.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for this worker (off by default).
func (w *Worker) SetWebSearch(o WebSearchOpts) { w.webSearch = o }

// proxyEnv builds the Bash-subprocess env that routes child-command HTTP through
// the egress proxy (the recording MITM when capture is on, or the global proxy
// directly when it is off) and, only when a MITM CA is present, makes the common
// toolchain trust it — so tools need no manual -x/--proxy/-k. Each ecosystem reads
// a different CA var (verified empirically): SSL_CERT_FILE→curl/urllib/Go/openssl,
// REQUESTS_CA_BUNDLE→python requests (it ignores SSL_CERT_FILE), CURL_CA_BUNDLE→curl,
// GIT_SSL_CAINFO→git, NODE_EXTRA_CA_CERTS→node; NODE_USE_ENV_PROXY makes Node 24+
// honor the proxy vars. ALL_PROXY is set too so a socks5 egress proxy (which curl
// only reads from ALL_PROXY, not HTTP(S)_PROXY) works in the capture-off path.
// Empty proxyAddr → nil (direct, unchanged env).
func proxyEnv(proxyAddr, caCert string) []string {
	if proxyAddr == "" {
		return nil
	}
	env := []string{
		"HTTP_PROXY=" + proxyAddr, "HTTPS_PROXY=" + proxyAddr,
		"http_proxy=" + proxyAddr, "https_proxy=" + proxyAddr,
		"ALL_PROXY=" + proxyAddr, "all_proxy=" + proxyAddr, // socks5 egress: curl reads only this
		"NODE_USE_ENV_PROXY=1", // Node 24+: honor HTTP(S)_PROXY in built-in fetch/http
	}
	if caCert != "" {
		env = append(env,
			"SSL_CERT_FILE="+caCert,
			"CURL_CA_BUNDLE="+caCert,
			"REQUESTS_CA_BUNDLE="+caCert,
			"GIT_SSL_CAINFO="+caCert,
			"NODE_EXTRA_CA_CERTS="+caCert,
		)
	}
	return env
}

// The trafficTool block and the Intermediate Output Rules are NOT here — they are code-owned and
// appended by workerSystem after rendering (section [B]/[C]), so editing the DB body can never drop
// them.
const workerDefaultTmpl = `You are a "work agent" of a cybersecurity platform authorized penetration testing system. You have been given an itemsintent (a phrase of direction to explore) with the sole responsibility of: ** completing this itemsintend, writing the finding back to the knowledge map, and then stopping returning. **

** Boundary (red line)**:
1. ** Just do the itemsintent you received**. ** If you see clues outside of this intent that are worth digging into** (paths to error leaks, possible points of connection with other assets, suspected entries in chains using other items),** check the summary of the fact and give it to the planner**.
2. The initial blockage (payload filtered / 404 / infusion without reflection) does not mean that it has been detected - to get all of Bent's bypasses back to the conclusion;
3. Operation only within authorized limits. If the system alert top is attached to [operational constraints], that is the highest priority red line: every command/detection is self-checked before being executed, and the breach is not done (even if it falls into the intent you have received).

** Write back at the end of the finding ** (it's in the drawing, it's not in the brain/text; for every result you write immediately, don't save until the number of steps is exhausted).
- ** New asset/resource → insert_assets (asset diagram)**: all the assets such as subdomain / service / endpoint / fingerprint / credentials. ** Here only register the asset; findings/judgments are not written here, use record_fact. **
- **Exploring conclusions/facts → record_fact (exploring diagram, transmitting intent_id)**: use it all.** Multiple observations summarized into [one items] facts** (summary a summary sentence + detail writing an extension of the summary, depending on the real execution process), do not have one property one item, one intent usually only one item; fragmenting will make the graph infinitely expanded –**default writes one item and merges all that can be merged into detail**; only when there are [completely independent, unmergeable] conclusions use facts multiple sub-items, this is a very rare exception, not usual.**Write only increments**: remember only this time [newly obtained], do not reword existing facts (just confirm that they already exist, no new additions need to be remembered).**Writ only what you really saw**: give evidence (one line: command + one or two lines of proof output, concise, details in detail), mark confidence (observed=directly seen / inferred=inferred from the phenomenon).
- **Confirm Vulnerability → report_finding (Discovery Map, Including PoC, Transmit intent_id)**: **Only use if you have actually triggered the vulnerability and obtained reproducible evidence (request/response or command output)**. Do not make "version/fingerprint matches to CVE"" appear to be injectable into ""external vulnerabilities library/update logs/code diff inferences"" when confirmed, nor do not substitute actual triggering with checking CVE library or comparing patch versions.


When you complete Bent, you summarize in one sentence what you did and what you wrote back.`

// The workerTrafficBlock is section [B]: the traffic-tool note, code-injected only when traffic capture
// (recording) is on — i.e. the traffic_* tools actually exist. Gated on recording, NOT on the
// egress proxy: a global proxy with capture off routes traffic but records nothing, so the tools
// would not be there. Not stored, not editable.
func workerTrafficBlock(recording bool) string {
	if !recording {
		return ""
	}
	return "\n\n**Flow tools**: \n- traffic_search / traffic_get/traffic_blob: look back on responses, find resources that have been visited, check traffic first, do not repeat curl the same URL**. traffic_search **must specify host**, default returns only 3 items very lightweight indexing (id/method/url/status/resp_len, no response content), requires more explicit capitalization limit; can be used with body_contains Full text search in request/response body (at least 3 characters, support substring and Chinese, such as password/key/error report/intranet address); to see an item's original language use traffic_get(id), where the super large body is displayed as @blob sha256:<hash>, use traffic_blob(hash) paragraph for full text."
}

// ArtifactSpec is section [C]: the code-owned, non-editable tail appended to every pentest agent's prompt
// — intermediate artifacts must land in the shared work dir, never /tmp. Guaranteed present
// regardless of how the DB body is edited.
func artifactSpec(dir string) string {
	return "\n \n** Statute for the Export of Intermediate Products**: Scripts, Payload, seized responses, provisional data, all intermediate products** are always recorded in this task directory** " + dir + "** (relative path is written here, and the absolute path can be used) - ** Do not write/tmp, do not use other absolute path**."
}

// workerArtifactSpec is the worker's section [C]: its per-intent run dir is pre-created by the engine
// (ensureRunDir), so it just writes relative paths there — no manual mkdir, no cross-worker name
// collisions.
func workerArtifactSpec(runDir string) string {
	return "\n \n** Statute for the Export of Intermediate Products**: Scripts, Payload, seized responses, provisional data, etc. " + runDir + "** (Automatedly built, written here directly by relative path without manual cataloging) - **No writing/tmp, no other absolute path**"
}

// ensureRunDir builds and creates an agent's working directory under base:
// <base>/tasks/<taskID> for planner/main; <base>/tasks/<taskID>/i<intentID> for a
// worker (intentID<=0 → task dir only). The "tasks/" segment groups per-task dirs
// symmetrically with the chat agent's "sessions/<sessionID>". Best-effort mkdir — on
// failure, writes fail the same way an unwritable CWD would.
func ensureRunDir(base string, taskID, intentID int64) string {
	dir := filepath.Join(base, "tasks", strconv.FormatInt(taskID, 10))
	if intentID > 0 {
		dir = filepath.Join(dir, "i"+strconv.FormatInt(intentID, 10))
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// cmdOutDir is the SDK large-tool-output spill dir under an agent's run dir.
func cmdOutDir(dir string) string { return filepath.Join(dir, "cmd-output") }

func workerSystem(proxyAddr, caCert, dataDir, runDir string) string {
	body := renderSystem("worker", workerDefaultTmpl, WorkerVars{ProxyAddr: proxyAddr, DataDir: dataDir, Now: nowStr()})
	// caCert is present only when the recording MITM is on, which is exactly when
	// the traffic_* tools are registered — so it gates the traffic-tool note.
	// Optional finding guidance is added for every role after tool resolution.
	return body + workerTrafficBlock(caCert != "") + workerArtifactSpec(runDir)
}

// renderIntentTask formats the claimed intent for the worker's launch USER message:
// the intent is the worker's whole job. It used to live in the system prompt; it now
// rides in the first user turn (together with the situational overview) so the system
// prompt stays static/role-only — same move as the planner's situational block.
// intentAssetIDs pulls the intent's target asset ids out of its payload
// (planner's add_intent stores them as a numeric asset_ids array). nil on absence
// or malformed payload.
func intentAssetIDs(intent *db.Node) []int64 {
	if intent == nil {
		return nil
	}
	var p struct {
		AssetIDs []int64 `json:"asset_ids"`
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return nil
	}
	return p.AssetIDs
}

func renderIntentTask(intent *db.Node) string {
	return fmt.Sprintf("\n\n【You received the intent (this time only task: do this item, only generate facts, stop when done)】:\n%s\nintent id: %d(write back it when record_fact / report_finding)", string(intent.Payload), intent.ID)
}

// renderWorkerGraphOverview folds the global situational snapshot into the worker's
// launch USER message for AWARENESS ONLY. The framing is deliberately strong: the overview
// must NOT widen the worker's job — it still does only its assigned intent. Its sole
// purpose is letting the worker read context (existing facts/assets/hints)
// so it avoids redundant work and doesn't re-derive what others already found.
func renderWorkerGraphOverview(data map[string]any) string {
	// The coverage is a signal to the planner to decide "which type of measurement is less /
	// whether to expand the range", which conflicts with the worker's responsibility boundary "just
	// do what you get, don't go after the uncovered points" → removed from the worker view.
	delete(data, "coverage")
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back silently: the worker just won't have the global context
	}
	return "\n \n Global exploration posture (read only, help you put your intent into the big picture): \n" +
		"Here's an overview of the whole Task exploration. The uses are two: first, to know what other people have found, and not to repeat it; second, to think about their relationship with the whole world when you look at your own indentation. \n" +
		"**Dispersion is good**: While exploring the intent, think deeply and associate. The only limit is that you don't actually execute another intent (that's a different worker's thing, scheduled by the planner). But whenever you come up with valuable clues (cross-asset connections, suspected use of other items in the chain, suspicious points at the global level), **make sure to write them into facts for the planners** - this is your important output, not nothing." +
		string(b)
}

// Execute runs one intent. hooks (the per-task Guard) gates every tool call; may
// be nil. emit, if non-nil, receives one ActivityRecord per execution step.
// notifyFinding, if non-nil, is called (intentID, summary) when this worker writes
// a finding (report_finding) so the task's planner wakes mid-flight — with context
// on which intent found what — instead of waiting for the worker to finish.
// Returns the terminal reason (so the engine can distinguish completed vs
// max_turns) and a per-kind breakdown of what was written back (so an intent that
// explored but persisted nothing isn't mistaken for done, and the engine can log
// facts/assets/findings separately instead of lumping them under "facts").
func (w *Worker) Execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string)) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, "", "")
}

// ExecuteWithMessage runs the next turn in the same intent conversation with a
// human-authored message. The HTTP handler does not edit the transcript;
// agentcore records the message as a normal user turn when this Worker starts.
// This keeps Worker continuation identical to the regular agent chat flow.
func (w *Worker) ExecuteWithMessage(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, strings.TrimSpace(requestID), strings.TrimSpace(message))
}

func (w *Worker) execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	tsx := NewToolSet(ts, name)
	tsx.SetFindingRecorder(w.findingRecorder)
	tsx.SetTaskID(taskID)
	coverageEnabled := as == nil || as.CoverageEnabled(taskID)
	tsx.SetCoverageEnabled(coverageEnabled)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetOwnerNode(intent.ID)         // assets this worker discovers anchor to its intent → visible to the task
	tsx.SetEnrich(enr)                  // async DNS/HTTP auto-completion for assets this worker writes
	tsx.SetNotifyFinding(notifyFinding) // report_finding : wakes up the planner when it falls, with the intent and finding.
	// base = built-in worker tools ∪ host tools (traffic) ∪ default tools (incl. Bash);
	// then augment with the agent's visible skills/MCP. During the SDK settlement
	// phase, Bash is hidden via Settlement.DisabledTools (no local gating needed).
	base := append(tsx.WorkerTools(), w.extraTools...)
	// Worker deliberately does not provide MultiEdit/Glob/Grep: file refinement for Edit, retrieve
	// Bash (grep/find), converge toolbar, reduce low-value calls. The rest of the SDK default tools
	// (Read/Write/Edit/LS/Bash/Sleep) are as usual.
	base = append(base, defaultToolsExcept("MultiEdit", "Glob", "Grep")...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts), IntentID: intent.ID})
	tools, def, cleanup := AugmentTools(ctx, "worker", base)
	defer cleanup()

	// The intention is that the worker [the only duty, invariant throughout the run] → puts the
	// target asset anchored to the intent with the initialization command and the raw data into the
	// system prompt: system reassembles every run, never suppressed by compaction, intends to be
	// present forever in long runs, and does not depend on the transcript history to keep the first
	// message when rerunning. At the cost of the system mixing in per-intent variable data, losing
	// cross-intent cache reuse; this is a deliberate tradeoff (the loss of the intention is much
	// more serious than saving tokens).
	runDir := ensureRunDir(w.workDir, taskID, intent.ID)
	// The run-wide intent is not the current tool action. Do not forward it or
	// inherit a parent run's background into the action reviewer.
	ctx = intercept.WithReviewContext(ctx, runDir, intercept.ReviewBackground{})
	overview := renderWorkerGraphOverview(tsx.graphOverviewData())
	sysBody := workerSystem(w.proxyAddr, w.proxyCACert, w.workDir, runDir)
	if w.wantConstraints() {
		sysBody += constraintBlock(ts) // Inject operation constraints (if any) into the system prompt, which the worker strictly follows when executing
	}
	// Intent block → Intent anchor asset block → Initialize instructions, successively added to the
	// system tail (same addition as constraintBlock).
	sysBody += renderIntentTask(intent)
	if as != nil {
		if ids := intentAssetIDs(intent); len(ids) > 0 {
			if assets, err := as.GetByIDs(ids); err == nil && len(assets) > 0 {
				if b, err := json.Marshal(assets); err == nil {
					sysBody += "\n \n Benent asset_ids corresponding targetset: \n" + string(b)
				}
				// These assets are explicitly targeted → automatically included in the task test
				// scope (same set of conservative granularity as insertAssets). The ON CONFLICT DO
				// NOTHING + uq_task_scope unique index of upsertTaskScope ensures no duplicate
				// additions; rerun/retry is also a no-op.
				if coverageEnabled {
					for _, a := range assets {
						_ = as.AddAutoScope(taskID, a.Type, a.Domain, a.URL, a.IP)
					}
				}
			}
		}
	}
	sysBody += "\n\n starts executing the intent above: just do it, only generate facts, assets, finding, stop when done."
	system, boundary := deferredSystem(sysBody, def)
	// The task-level deadline (injected by ctx) presses the wall clock budget of this run + decides
	// the ending word (see taskclock.go).
	tc := taskClockFrom(ctx)
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, w.runTimeout)
	settle := wrapupSettlement("worker", []string{"Bash"})
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("worker", []string{"Bash"}, clamped)
	}
	opts := agentcore.Options{
		Provider:        w.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		// WebFetch logs the proxy, whose HTTP is traced as curl; loads the proxy CA to allow
		// MITM-resigned HTTPS certificates to pass normal validation (instead of turning off
		// validation).
		EnableWebFetch: true,
		WebFetchProxy:  w.proxyAddr,
		WebFetchCACert: w.proxyCACert,
		// Web search (optional). dggs requires no key; brave-free requires BraveKey; tavily
		// requires TavilyKey. WebSearchProxy is a standalone export proxy (http/https/socks5) that
		// is independent of the MITM proxy that records traffic; empty is direct.
		EnableWebSearch:       w.webSearch.Enabled,
		WebSearchBackend:      w.webSearch.Backend,
		BraveSearchAPIKey:     w.webSearch.BraveKey,
		TavilySearchAPIKey:    w.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: w.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  w.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   w.webSearch.DeepSeekModel,
		WebSearchProxy:        w.webSearch.Proxy,
		// The HTTP default for the Bash subcommand is go to register agent + trust its CA (the tool
		// does not require -x/-k).
		BashEnv:    proxyEnv(w.proxyAddr, w.proxyCACert),
		WorkingDir: runDir,
		MaxTurns:   w.maxTurns, // 0 = unlimited (configurable in agent management)
		// Wall clock budget, round boundary, no half-way; 0 = unlimited.If there is a task-level
		// deadline, push it to min (your own budget, left from the deadline), so that the run will
		// end naturally when the task reaches point (see taskclock.go).
		MaxDuration: maxDur,
		// Hit the budget (run OR time) → SDK runs a round end (hidden bash), writes back the
		// identified ones, avoiding bad tails. When clamped (by task deadline) use PromptByReason:
		// because timeout=task to point→ task timeout word, because steps=clamp window steps first
		// exhausted→ fall per-run word. Non-clamped keep pure per-run.
		Settlement: settle,
		// large tool output spills to cmd-output/ with a head + pointer (SDK tool.Capture);
		// full output preserved on disk. The truncation limit is the SDK default (30000
		// characters).
		ToolOutputDir: cmdOutDir(runDir),
		Compaction:    compactionConfig(w.compactionWindow()), // long tool-heavy runs stay within the window
		Todos:         actool.NewTodoStore(),                  // Session-level To-Do (TodoWrite), for planning purposes only, exit-to-lose
		NonStreaming:  w.nonStreaming(),                       // Provider.Complete when the profile is non-fluid
		MaxTokens:     w.maxTokens(),                          // 0 = no limit, determined by the server default value
	}
	if hooks != nil { // typed-nil guard: only set when concrete (avoids harness panic)
		opts.Hooks = hooks
	}
	if w.tx != nil { // persist raw LLM conversation; one file per worked intent
		opts.Transcript = w.tx
		opts.SessionID = WorkerSessionID(ts.ID(), intent.ID)
	}
	intentID := intent.ID
	emitWrap := func(r db.Activity) {
		if emit != nil {
			r.NodeID, r.Worker = &intentID, name
			emit(r)
		}
	}
	// Intent / boot command / intent anchor assets are sent with the system prompt (see sysBody
	// assembly above). This boot user message only carries the global overview — a degradeable
	// overview information that can be suppressed without hindrance. overview When marshal rarely
	// fails to fill, returns a boot word to avoid an empty user message in the first round.
	input := overview
	if strings.TrimSpace(input) == "" {
		input = "Start executing the intent received in the system: just do it, only generate facts, assets, finding, stop when done."
	}

	// Experimental feature: context compression is taken over by noa after it is enabled (archives
	// are centralized under <workDir>/noa/<SessionID>, permanently).
	noaSession := WorkerSessionID(ts.ID(), intent.ID)
	enableNoa(&opts, w.noaEnabledFn, w.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close() // release the session's background-task manager (temp dir + processes)

	// Resume prior conversation if this intent was paused/blocked/exhausted and is
	// being re-run. The transcript ID is deterministic per intent, so if a prior
	// session exists the worker continues from where it left off instead of
	// restarting from scratch.
	alreadyRecorded := false
	if w.tx != nil {
		_ = s.Resume(opts.SessionID)
		alreadyRecorded = requestID != "" && hasWorkerChatMessage(s.Messages(), requestID)
		if len(s.Messages()) > 0 && message == "" {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
			input = "Implementation continued."
		} else if len(s.Messages()) > 0 {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
		}
	}
	if message != "" {
		if alreadyRecorded {
			input = "Continues to implement the last manual dialogue input. Do not repeat completed actions."
		} else if len(s.Messages()) > 0 {
			input = workerChatMarker(requestID) + "\n [New intent for manual dialogue] \n" + message +
				"\n \n Please apply this manual input immediately before deciding whether or not the original task needs to be continued, depending on the context."
		} else {
			input += "\n\n" + workerChatMarker(requestID) + "\n [New intent for manual dialogue] \n" + message +
				"\n \n Please prioritize the implementation of this manual input."
		}
	}

	// Budgets + settlement are owned by the SDK (MaxTurns/MaxDuration + Settlement):
	// on hit it runs a wrap-up turn and finishes with ReasonMaxTurns/ReasonTimeout.
	// MaxDuration now interrupts an in-flight tool at the wall-clock deadline and
	// enters the wrap-up phase on the live ctx, so a run whose tool overran the budget
	// still settles (no external hard-timeout backstop needed). ctx itself carries only
	// pause / planner kill / shutdown, which the engine distinguishes and re-queues/stops.
	_, reason, err := captureRunSession(ctx, s, input, emitWrap)
	return reason, tsx.Writes(), err
}
