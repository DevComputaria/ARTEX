package notify

// Snapshot is a contract for the notification_events.snapshot JSONB column. It is written by the db
// layer's vulnerability storage transaction and read by the server layer's delivery engine to match
// the filter.
//
// Why store the vulnerability field redundantly and not check it back during rendering: The
// vulnerability is renamed, leveled, and status changed afterwards, and the push content should
// reflect the conclusion at the time of the ** event** - the checkback would be dangerously
// misleading as "replaced to low afterwards".
type Snapshot struct {
	// Event type: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// Only kind=finding_status_changed is not empty.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item is a vulnerability to be pushed for channel rendering.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets is the display name of the parsed asset (e.g. domain/IP).
	Assets []string
	// DetailURL is the back-chain of the vulnerability details; empty means no public_base_url
	// assigned, and is omitted during rendering.
	DetailURL string
	// Specific to state change events; both are rendered as "pending → fixed" in non-space time.
	FromStatus string
	ToStatus   string
}

// IsStatusChange reports whether the entry is a state change event.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title Returns the display title of the entry: preferably a manually named name, returning the
// vulnerability type vulnclass, both with a placeholder when empty — never output an empty header.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(未命名漏洞)"
}

// Message is the entire content sent by a channel.
type Message struct {
	// The length is 1 for single-string pushes; a whole batch for digests. Blank slices are illegal
	// and the calling party must guarantee at least one.
	Items []Item
	// When Batch=true, render as summary message (with new title, time window and bar number).
	Batch bool
	// WindowMinutes is the aggregate cycle (minutes), used only when Batch=true for "near N
	// minutes" cases. Intentionally passed by configuration explicitly rather than rendering
	// time.Since: Rendering remains certain, so it is better to measure.
	WindowMinutes int
	// HomeURL is the platform panel address (global public_base_url); empty does not contain a
	// panel entry.
	HomeURL string
}
