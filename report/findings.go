package report

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// Rendering for the find page export: Render a row of the findings table as summary Markdown,
// single-string Markdown, or CSV. JSON is serialized directly by the server layer with DTO and is
// not here.

// sortFindingsForExport Sorts down by severity and then backwards in time, consistent with the
// grouping in the summary report.
func sortFindingsForExport(fs []*db.DBFinding) {
	sort.SliceStable(fs, func(i, j int) bool {
		ri, rj := sevRank[fs[i].Severity], sevRank[fs[j].Severity]
		if ri != rj {
			return ri < rj // The smaller the risk
		}
		return fs[i].CreatedAt.After(fs[j].CreatedAt)
	})
}

// findingTitle Gets the vulnerability's readable title: Name → Category → Unclassified.
func findingTitle(f *db.DBFinding) string {
	return nz(f.Name, nz(f.VulnClass, "Not classified"))
}

// FindingsMarkdown Consolidates a set of findings into one summary report (summary + grouping by
// severity level, each with category/state/task/evidence/detailed report).
func FindingsMarkdown(fs []*db.DBFinding, generatedAt time.Time) string {
	items := append([]*db.DBFinding(nil), fs...)
	sortFindingsForExport(items)

	var b strings.Builder
	b.WriteString("# Vulnerabilityfinding Summary Report \n \n")
	fmt.Fprintf(&b, "- ** Generation time** %s \n", generatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- **finding total**: %d \n", len(items))

	// Summary: Counts each severity level.
	counts := map[string]int{}
	for _, f := range items {
		counts[f.Severity]++
	}
	b.WriteString("## Summary \n \n")
	b.WriteString("\n--- ---- \n")
	for _, s := range []struct{ key, label string }{
		{"critical", "Serious"}, {"high", "High-risk"}, {"medium", "We're in danger."}, {"low", "Low risk."},
	} {
		fmt.Fprintf(&b, "| %s | %d |\n", s.label, counts[s.key])
	}
	b.WriteString("\n")

	if len(items) == 0 {
		b.WriteString("_No matching vulnerabilities._\n")
		return b.String()
	}

	b.WriteString("## Vulnerabiliti \n \n")
	for i, f := range items {
		fmt.Fprintf(&b, "### %d. [%s] %s\n\n", i+1, strings.ToUpper(nz(f.Severity, "info")), findingTitle(f))
		if f.VulnClass != "" {
			fmt.Fprintf(&b, "- ** Category**: %s \n", f.VulnClass)
		}
		fmt.Fprintf(&b, "- ** Status**: %s \n", nz(f.Status, "pending"))
		if desc := strings.TrimSpace(f.TaskDescription); desc != "" {
			fmt.Fprintf(&b, "- **Task**: %s \n", desc)
		}
		fmt.Fprintf(&b, "- **finding time** %s \n", f.CreatedAt.Format("2006-01-02 15:04:05"))
		if s := strings.TrimSpace(f.Summary); s != "" {
			fmt.Fprintf(&b, "%s\n\n", s)
		}
		if e := strings.TrimSpace(f.Evidence); e != "" {
			fmt.Fprintf(&b, "**evidence:** \n \n `` `\n%s\n` `` \n \n", e)
		}
		if rep := strings.TrimSpace(f.Report); rep != "" {
			b.WriteString("** Detailed report:** \n \n")
			b.WriteString(rep)
			b.WriteString("\n\n")
		}
		b.WriteString(findingTrafficMarkdown(f, false))
		b.WriteString("---\n\n")
	}
	return b.String()
}

// SingleFindingMarkdown renders a single vulnerability as an independent Markdown (for "one file,
// one vulnerability" packaging).
func SingleFindingMarkdown(f *db.DBFinding, generatedAt time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# [%s] %s\n\n", strings.ToUpper(nz(f.Severity, "info")), findingTitle(f))
	if f.VulnClass != "" {
		fmt.Fprintf(&b, "- ** Category**: %s \n", f.VulnClass)
	}
	fmt.Fprintf(&b, "- ** Serious **: %s \n", nz(f.Severity, "info"))
	fmt.Fprintf(&b, "- ** Status**: %s \n", nz(f.Status, "pending"))
	if desc := strings.TrimSpace(f.TaskDescription); desc != "" {
		fmt.Fprintf(&b, "- **Task**: %s \n", desc)
	}
	fmt.Fprintf(&b, "- **finding time** %s \n", f.CreatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- ** Generation time** %s \n", generatedAt.Format("2006-01-02 15:04:05"))
	if s := strings.TrimSpace(f.Summary); s != "" {
		fmt.Fprintf(&b, "## Overview \n \n %s\n \n", s)
	}
	if e := strings.TrimSpace(f.Evidence); e != "" {
		fmt.Fprintf(&b, "## event \n \n `` `\n%s\n` `` \n \n", e)
	}
	if rep := strings.TrimSpace(f.Report); rep != "" {
		b.WriteString("## Detailed report \n \n")
		b.WriteString(rep)
		b.WriteString("\n")
	}
	b.WriteString(findingTrafficMarkdown(f, true))
	return b.String()
}

var unsafeFilenameChars = regexp.MustCompile(`[^\p{Han}\p{L}\p{N}._-]+`)

// FindingFilename generates a safe.md filename for "one file per vulnerability" in the form of
// `critical_SQL_inject_#123.md`. Remove path separators and control characters to avoid illegal
// paths within zip.
func FindingFilename(f *db.DBFinding) string {
	sev := nz(f.Severity, "info")
	title := findingTitle(f)
	name := fmt.Sprintf("%s_%s_#%d", sev, title, f.ID)
	name = unsafeFilenameChars.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._")
	if name == "" {
		name = fmt.Sprintf("finding_%d", f.ID)
	}
	// Defensive: Peel off a layer of path, eliminate zip slip.
	name = path.Base(name)
	if len(name) > 120 {
		name = name[:120]
	}
	return name + ".md"
}

// FindingsCSV Renders a set of findings as CSV (with UTF-8 BOM, so Excel can recognize Chinese
// correctly). Does not contain large report/evidence text, only summary type fields; requires full
// text export in Markdown/JSON.
func FindingsCSV(fs []*db.DBFinding) []byte {
	items := append([]*db.DBFinding(nil), fs...)
	sortFindingsForExport(items)

	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF") // UTF-8 BOM
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"ID", "Name", "Category", "Serious Level", "Status", "butsk", "Finding time", "Overview", "Number of traffic transactions", "FlowvedenceID"})
	for _, f := range items {
		_ = w.Write([]string{
			fmt.Sprintf("%d", f.ID),
			findingTitle(f),
			f.VulnClass,
			nz(f.Severity, "info"),
			nz(f.Status, "pending"),
			f.TaskDescription,
			f.CreatedAt.Format("2006-01-02 15:04:05"),
			strings.TrimSpace(f.Summary),
			fmt.Sprint(len(f.TrafficBindings)), findingTrafficIDs(f),
		})
	}
	w.Flush()
	return buf.Bytes()
}

func findingTrafficIDs(f *db.DBFinding) string {
	ids := make([]string, 0, len(f.TrafficBindings))
	for _, b := range f.TrafficBindings {
		ids = append(ids, fmt.Sprint(b.ID))
	}
	return strings.Join(ids, ",")
}

func findingTrafficMarkdown(f *db.DBFinding, attachments bool) string {
	stale := f.Report != "" && f.EvidenceVersion != f.ReportEvidenceVersion
	if len(f.TrafficBindings) == 0 && !stale {
		return ""
	}
	var out strings.Builder
	out.WriteString("\n# Associated flow evidence \n \n")
	fmt.Fprintf(&out, "Version of event: %d; Quantities tied: %d \n \n", f.EvidenceVersion, len(f.TrafficBindings))
	if stale {
		out.WriteString("The event has been changed and the detailed report is to be updated. \n \n")
	}
	for i, b := range f.TrafficBindings {
		fmt.Fprintf(&out, "%d. **evidence#%d * %s ** `%s %s`, status code %d \n", i+1, b.ID, b.Role, b.Snapshot.Method, strings.ReplaceAll(b.Snapshot.URL, "`", "%60"), b.Snapshot.Status)
		if b.Note != "" {
			fmt.Fprintf(&out, "   %s\n", strings.ReplaceAll(b.Note, "\n", "\n   "))
		}
		if attachments {
			fmt.Fprintf(&out, "   [Request for information] (evidence/%d/%d/request.http) . [Responsive communication] (evil/%d/%d/response. http) \n", f.ID, b.ID, f.ID, b.ID)
		}
	}
	out.WriteString("\n")
	return out.String()
}
