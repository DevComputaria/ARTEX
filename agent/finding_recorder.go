package agent

import (
	"context"

	"github.com/Autumn-27/artex/db"
)

// FindingRecorder is injected by the host; agents never synthesize or copy
// evidence bodies themselves. Its implementation owns the atomic write.
type FindingRecorder interface {
	Record(context.Context, db.RecordFindingInput, []db.TrafficRef) (*db.RecordedFinding, error)
}

// Tool-use guidance is appended without replacing the user's editable prompt.
// It does not require capture or claim that unavailable traffic tools exist.
const findingTrafficGuidance = "\n\n**vulnerability traffic evidence (optional)**: When calling report_finding to report a vulnerability, if there are HTTP requests/responses that have been viewed and confirmed to support the vulnerabilities conclusion, you can use traffic_refs to bind the real ID in the sequence of replication; domain name and time are only for candidate screening, no assumption of association. Omit or pass [] when non-HTTP vulnerabilties such as TCP are not captured or do not match exactly, keep command output, logs, etc. other verifiable evidence in the evidence, recommending the reason for unbinding. Do not guess the ID, nor do not just repeat detection for repackaging."

func (t *ToolSet) SetFindingRecorder(r FindingRecorder)   { t.findingRecorder = r }
func (w *Worker) SetFindingRecorder(r FindingRecorder)    { w.findingRecorder = r }
func (p *Planner) SetFindingRecorder(r FindingRecorder)   { p.findingRecorder = r }
func (m *MainAgent) SetFindingRecorder(r FindingRecorder) { m.findingRecorder = r }
