package agent

import (
	"strings"

	"github.com/Autumn-27/artex/db"
)

// constraintBlock renders this task's operation constraints (task_constraints) as a
// high-priority block appended to the planner/worker system prompt. allow/deny are
// grouped; empty string when there are no constraints (or ts is nil). The framing
// deliberately puts these ABOVE the exploration/expansion heuristics so a declared
// boundary wins the tug-of-war against "chase another entry surface".
func constraintBlock(ts *db.ExplorationStore) string {
	if ts == nil {
		return ""
	}
	rows, err := ts.ListConstraints()
	if err != nil || len(rows) == 0 {
		return ""
	}
	var allow, deny []string
	for _, c := range rows {
		text := strings.TrimSpace(c.Text)
		if text == "" {
			continue
		}
		if c.Kind == "allow" {
			allow = append(allow, "- "+text)
		} else {
			deny = append(deny, "- "+text)
		}
	}
	if len(allow) == 0 && len(deny) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n \n [Operational constraints (highest priority, over all exploration/exploitation below; self-censorship must be exercised before each action is performed)]:")
	if len(allow) > 0 {
		b.WriteString("\n Allowed operation: \n")
		b.WriteString(strings.Join(allow, "\n"))
	}
	if len(deny) > 0 {
		b.WriteString("\n Prohibited operations: \n")
		b.WriteString(strings.Join(deny, "\n"))
	}
	b.WriteString("\n (Finding a new target/port/host outside the finding constraint does not amount to authorization: unless it falls within the above permissions, it is counted as an out-of-scope fact and skipped, with no intent or execution action derived for it.)")
	return b.String()
}
