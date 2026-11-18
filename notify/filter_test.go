package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// Tortuous JSON, empty inputs, incorrect field types—all must be degraded to zero-value Filter,
	// i.e. "no filtering." This invariant is the downside of "better push than miss": Once this is
	// changed to error or semi-parsing, the user will silently drop all high-risk notifications
	// with one character wrong.
	cases := []struct {
		name string
		raw  string
	}{
		{"Empty input", ""},
		{"Illegal JSON", `{not json`},
		{"Interrupted JSON", `{"min_severity":`},
		{"Type does not match", `{"min_severity": 123, "task_ids": "abc"}`},
		{"The top level is the array.", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("Deform configuration should be converted to zero, Filter, get %+v", f)
			}
			// The zero-value Filter must hit any event.
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("Zero, Filter hits all events.")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// The unknown level has a sequence number of 0 and should be blocked by any non-empty
		// threshold (not taken in doubt).
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("Min = %q sev =%q: Desire %v Get %v", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQL Injection",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"Empty range = unlimited", Filter{}, true},
		{"Task's hit.", Filter{TaskIDs: []int64{7}}, true},
		{"Task's not hit.", Filter{TaskIDs: []int64{8}}, false},
		{"Task's got a lot of hits.", Filter{TaskIDs: []int64{8, 7}}, true},
		{"There's a cross-section in the asset.", Filter{AssetIDs: []int64{20, 99}}, true},
		{"Aset no intersection", Filter{AssetIDs: []int64{99}}, false},
		{"Task hit it with an asset.", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"Task hit, but Aset didn't hit.", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("Expect %v Get %v", tc.expect, got)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"Include is empty = all", Filter{}, "Any type", true},
		{"Include hit", Filter{VulnClassInclude: []string{"SQL"}}, "SQL Injection", true},
		{"Include missed.", Filter{VulnClassInclude: []string{"Command execution"}}, "SQL Injection", false},
		{"Include, multiple words hit any of them.", Filter{VulnClassInclude: []string{"Command execution", "SQL"}}, "SQL Injection", true},
		{"Case insensitive", Filter{VulnClassInclude: []string{"sql"}}, "SQL Injection", true},
		{"Exclude by impact", Filter{VulnClassExclude: []string{"Information leaks"}}, "Information leaks", false},
		{"Exclude Unhit Release", Filter{VulnClassExclude: []string{"Information leaks"}}, "SQL Injection", true},
		// Exclusion takes precedence over inclusion: both should be out of the game.
		{"Exclusion over inclusion", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"Injection"},
		}, "SQL Injection", false},
		// Blank keywords should be ignored, otherwise they degrade to "match all strings containing
		// spaces".
		{"Blank keyword ignored", Filter{VulnClassInclude: []string{"", "  "}}, "SQL Injection", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("Expect %v Get %v", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// Default off: Most people say "push vulnerability" when they are talking about newly
	// discovered vulnerabilities, not state-of-the-art.
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("The state change event should be skipped when it is not opened")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("Start on_status_change Post-state change event hit")
	}
	// The creation of events is not affected by on_status_change.
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("Create event should not depend on on_status_change")
	}
}
