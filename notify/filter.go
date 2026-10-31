package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter is the contract of the JSONB column notification_channels.filter: the filtering condition
// for the channel instance. All fields are optional, default is "no filter" — this is the bottom
// line of the distorted configuration, see ParseFilter.
type Filter struct {
	// MinSeverity is the lowest level threshold (low/medium/high/critical), empty = no threshold.
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs are empty arrays representing unlimited; non-empty requires that the event
	// intersects with it.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude is empty for all inclusions; non-empty requires vulnclass to match any of
	// the keywords. VulnClassExclude matches any of the keywords for exclusions (exclusion takes
	// precedence over inclusion).
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange determines whether the channel receives a vulnerability state change event
	// (meaningful only in realtime mode).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter Parses the channel filter configuration.
//
// **Never return error.** This is a deliberate design choice: Filter always degrades to zero when
// filter conditions are configured incorrectly (= no filtering = all hits), because for a
// vulnerability notification system, it is better to push one more than to silently miss a
// high-risk one.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// If the parsing fails, f is kept to zero, i.e. no filtering.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity reports whether s is a valid threshold level (empty string indicates no
// threshold).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate Validates the fields in the filter configuration that are **value-restricted** for use
// when saving a channel.
//
// Why write-only blocking is necessary: Match determines that the unknown threshold is `rank >= 0`,
// so it is always true - i.e., if min_severity is mistyped by one word ('hgih'), the filter will
// **silent fail** and become "all push". This is consistent with the package's trade-off of "better
// push than miss push" (it does not miss), but the consequence is that the user thinks he is doing
// a rank push and actually inserts all vulnerabilities into the group, and there is no indication
// that he has been mismatched.
//
// Note that Validate is only used for **writing** paths. The read path still follows ParseFilter's
// tolerant semantics, so that bad values already present in the historical data do not make the
// entire channel unreadable.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("最低级别 %q 无效，可选：low / medium / high / critical，或留空表示不限", f.MinSeverity)
	}
	return nil
}

// Match determines whether an event should be delivered to a channel with the filtering condition.
//
// **Never return error**, reasons identical to ParseFilter: any internal exception is treated as a
// hit.
func Match(f Filter, s Snapshot) bool {
	// Only explicitly enabled channels receive state change events. The default is off, because
	// most users expect "push" to mean "discover new vulnerabilities", rather than a flow-through
	// follow-up of each state flow.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// Exclusion priority: Hit any excluding keyword and you are out of the game, even if you also
	// hit the list of included keywords.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// A linear scan of a small set is sufficient; the scales on both sides are "dozens of
	// hand-picked ones", and the cost of building a map is greater than the benefits.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold Reports whether s contains any keyword in keywords (case-insensitive).
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
