package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// This file is the channel configuration and event layer for IM pushes. For the receipt and status
// flow of a delivery task, see db/notification_delivery.go.
//
// Two invariants must be maintained when this document is revised:
//
// 1. The write transaction (RecordFindingTx) only calls InsertNotificationEventTx for a blind
// insert, does not read any related tables and does not make filter matches. Any read operations
// introduced here could contaminate or even stop the vulnerable write transaction due to
// user-incorrectly configured filter conditions.
// 2. Filter matches never fail: configuration anomalies are always treated as "hits" (see
// notify.Match).

// The page you are looking for does not exist.
var ErrNotificationChannelNotFound = errors.New("通知渠道不存在")

// Delivery status.
const (
	NotifyStatePending = "pending" // To be issued
	NotifyStateSending = "sending" // Has been collected by a dispatcher, lease has not expired
	NotifyStateSent    = "sent"    // Delivered
	NotifyStateFailed  = "failed"  // retry exhausted or permanently failed, can be re-sent manually
	NotifyStateSkipped = "skipped" // Channel has been disabled and no longer sent
)

// Push mode.
const (
	NotifyModeRealtime = "realtime"
	NotifyModeDigest   = "digest"
)

// ValidNotifyMode Whitelist validation push mode (similar to findings.status: no DB CHECK, for
// later extension).
func ValidNotifyMode(m string) bool {
	return m == NotifyModeRealtime || m == NotifyModeDigest
}

// Config and Filter keep the original JSON, parsing it to the notify package—the db layer does not
// understand their field meaning.
type NotificationChannel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Filter json.RawMessage `json:"filter"`
	// The Enabled pointer is used to distinguish between "do not pass this field" and "explicitly
	// pass false" — the front-end switch control submits only the field that has been changed.
	Enabled    *bool     `json:"enabled,omitempty"`
	RatePerMin int       `json:"rate_per_min"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsEnabled returns whether the channel is enabled; Enabled is treated as enabled if nil (not
// loaded).
func (c *NotificationChannel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NotificationEvent is an event fact.
type NotificationEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	FindingID int64           `json:"finding_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	CreatedAt time.Time       `json:"created_at"`
}

const notificationChannelCols = `id, name, kind, enabled, config, mode, filter, rate_per_min, created_at, updated_at`

func scanNotificationChannel(sc interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var enabled bool
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &enabled, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Enabled = &enabled
	return &c, nil
}

// ListNotificationChannels returns all channel instances, enabled in the first row and ranked by
// id. The ordering is put in SQL to ensure that the UI and dispatcher see the same stable order.
func (d *DB) ListNotificationChannels(ctx context.Context) ([]*NotificationChannel, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels
ORDER BY enabled DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		c, err := scanNotificationChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannelByID Gets a single channel.
func (d *DB) NotificationChannelByID(ctx context.Context, id int64) (*NotificationChannel, error) {
	row := d.QueryRowContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels WHERE id=$1`, id)
	c, err := scanNotificationChannel(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotificationChannelNotFound
	}
	return c, err
}

// SaveNotificationChannel Creates or updates a channel.
//
// The update only overrides the fields given by the call equation (not nil / non-empty), so that
// the front-end can submit a locally modified drawer form without returning the fields it did not
// display in config — returning which would cause the crash of "mask value overriding real key".
func (d *DB) SaveNotificationChannel(ctx context.Context, c *NotificationChannel) (int64, error) {
	if c.Mode == "" {
		c.Mode = NotifyModeRealtime
	}
	// Here, **no** processing is intentionally done on 0: 0 is a valid configuration, meaning
	// "unlimited flow".
	//
	// It was written as `if c.RatePerMin <= 0 { c.RatePerMin = default value }`, which meant "Give
	// a safe default if not specified", but that also swallowed up the "explicit set to 0" —
	// documentation, UI hints and takeTokens all interpreted 0 as unlimited, except for the quietly
	// changed 20 (DingTalk/Tesla/Telegram) or 100 (Feishu), which operators thought had released
	// the limit, actually being stuck at 20/minute without any hint.
	//
	// The difference between "unspecified" and "explicit 0" is only known by the caller (default
	// field in request body vs explicit 0), so the default value is filled by the server layer when
	// the field is defaulted, see notifyCreateChannel.
	if c.RatePerMin < 0 {
		return 0, errors.New("限流值不能为负")
	}
	if c.Config == nil {
		c.Config = json.RawMessage(`{}`)
	}
	if c.Filter == nil {
		c.Filter = json.RawMessage(`{}`)
	}
	enabled := c.IsEnabled()

	if c.ID == 0 {
		var id int64
		err := d.QueryRowContext(ctx, `INSERT INTO notification_channels(name,kind,enabled,config,mode,filter,rate_per_min)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin).Scan(&id)
		return id, err
	}
	res, err := d.ExecContext(ctx, `UPDATE notification_channels
SET name=$2, kind=$3, enabled=$4, config=$5, mode=$6, filter=$7, rate_per_min=$8
WHERE id=$1`,
		c.ID, c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotificationChannelNotFound
	}
	return c.ID, nil
}

// SetNotificationChannelEnabled Toggles on or off.
//
// When you disable a channel, mark all submissions that have not yet been sent as skipped:
// otherwise, when you re-enable it, you will suddenly receive a backlog of old vulnerabilities that
// are outdated and easily mistaken for new.
func (d *DB) SetNotificationChannelEnabled(ctx context.Context, id int64, enabled bool) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE notification_channels SET enabled=$2 WHERE id=$1`, id, enabled)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotificationChannelNotFound
		}
		if !enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET state=$2, last_error=$3
WHERE channel_id=$1 AND state IN ($4,$5)`,
				id, NotifyStateSkipped, "渠道已停用", NotifyStatePending, NotifyStateSending); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNotificationChannel Delete the channel. Its delivery history is deleted with the external
// key cascade (the channel configuration is gone, and the history cannot be read).
func (d *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM notification_channels WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotificationChannelNotFound
	}
	return nil
}

// RecordNotificationEventTx Trys to write a push event in the caller's business**.
//
// This is the only change related to notification in the vulnerability write path: one INSERT, no
// table reading, no channel recognition, no filtering. Transaction submission ensures that the
// "vulnerability falls" atom matches the "push task exists" atom, and there is no window where a
// successful submission is not queued and the message is permanently lost.
//
// Two key designs, neither of which was handwritten:
//
// 1. **Why use SAVEPOINT**: In PostgreSQL, any error in a transaction aborts the entire transaction
// and all subsequent statements (including COMMIT) fail. So "ignore this INSERT error and let the
// caller continue to submit" is not possible in PG — unless you use a savepoint to isolate the
// error on this statement. Without a savepoint, there is only one option left, "roll back the whole
// thing."
//
// 2. **Why the whole rollback is wrong**: Pushes are a convenience feature, vulnerability logs are
// the product itself. A problem with a notification table (old library not migrated, disk momentary
// failure) shouldn't keep high-risk vulnerabilities out of the repository. So here, isolating
// errors, logging, returning false, and letting the vulnerability be written to the commit as usual
// — at the cost of losing the push — is intentional. Returning bool instead of error is
// intentional: the caller should not treat it as an error that affects write success or failure.
func RecordNotificationEventTx(ctx context.Context, tx *sql.Tx, kind string, findingID int64, snap notify.Snapshot) bool {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[notify] 序列化推送事件失败 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notify_event`); err != nil {
		log.Printf("[notify] 建立保存点失败 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3)`,
		kind, findingID, string(raw)); err != nil {
		log.Printf("[notify] 写入推送事件失败 finding=%d（漏洞记录不受影响）: %v", findingID, err)
		// Returns the transaction from aborted to save point.
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notify_event`); rbErr != nil {
			log.Printf("[notify] 回滚到保存点失败 finding=%d: %v", findingID, rbErr)
		}
		return false
	}
	// Release save points to avoid accumulating useless save points in long operations.
	_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT notify_event`)
	return true
}

// AddNotificationEvent is a standalone transaction version of InsertNotificationEventTx for use by
// callpoints that are not in an existing transaction (such as the "send test message" channel,
// which has no real finding).
func (d *DB) AddNotificationEvent(ctx context.Context, kind string, findingID int64, snap notify.Snapshot) (int64, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("序列化通知事件快照失败: %w", err)
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3) RETURNING id`,
		kind, findingID, string(raw)).Scan(&id)
	return id, err
}

// FanOutPendingEvents expands unassigned vulnerability events into delivery tasks according to the
// channel currently enabled, returning the number of events processed in this round and the number
// of newly created deliveries.
//
// The whole operation is done in one task: events are picked up with FOR UPDATE SKIP LOCKED, and
// multiple processes running at the same time are picked up on different rows (the same technique
// is used for picking up archive queues in the project, see completeNextArchiveJob in
// db/task_archives.go).
//
// Filter matches are intentionally placed on the Go side rather than the SQL side: the filter
// criteria for channels is a JSONB of optional fields, and expressing six combinations of matches
// in SQL makes the query difficult to maintain, while the number of channels is "man-made", which
// means that it is faster and better to match row by row in memory after full load.
//
// An event that does not hit any channel is also marked fanned_out — otherwise it will remain in
// the pending collection forever, with each tick being swept over.
func (d *DB) FanOutPendingEvents(ctx context.Context, limit int) (eventCount, deliveryCount int, err error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // No-op after successful commit

	channels, err := listEnabledNotificationChannelsTx(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, finding_id, snapshot FROM notification_events
WHERE NOT fanned_out ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	var (
		events      []NotificationEvent
		parsedSnaps []notify.Snapshot
	)
	for rows.Next() {
		var ev NotificationEvent
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.FindingID, &ev.Snapshot); err != nil {
			rows.Close()
			return 0, 0, err
		}
		var snap notify.Snapshot
		// The snapshot is written by us, and must be parsed in theory; parsing failure does not
		// stop the submission process, but the event is skipped by all channels with filtering
		// conditions because the field is empty—better to push one line less than to let a bad row
		// block the entire queue.
		_ = json.Unmarshal(ev.Snapshot, &snap)
		// kind is based on the inline value: the snapshot is a render copy, possibly written by an
		// older version.
		snap.Kind = ev.Kind
		events = append(events, ev)
		parsedSnaps = append(parsedSnaps, snap)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(events) == 0 {
		return 0, 0, tx.Commit()
	}

	type pending struct {
		eventID   int64
		channelID int64
	}
	var toInsert []pending
	for i, snap := range parsedSnaps {
		for _, ch := range channels {
			if !notify.Match(notify.ParseFilter(ch.Filter), snap) {
				continue
			}
			toInsert = append(toInsert, pending{eventID: events[i].ID, channelID: ch.ID})
		}
	}
	if len(toInsert) > 0 {
		var (
			vals []string
			args []any
		)
		for _, p := range toInsert {
			vals = append(vals, fmt.Sprintf("($%d,$%d)", len(args)+1, len(args)+2))
			args = append(args, p.eventID, p.channelID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries(event_id,channel_id) VALUES `+strings.Join(vals, ","), args...); err != nil {
			return 0, 0, err
		}
	}

	// Marks the current round of events as assigned. Events that do not hit any channel are also
	// marked (see function note).
	ids := make([]string, 0, len(events))
	markArgs := make([]any, 0, len(events))
	for _, ev := range events {
		markArgs = append(markArgs, ev.ID)
		ids = append(ids, fmt.Sprintf("$%d", len(markArgs)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_events SET fanned_out=true WHERE id IN (`+strings.Join(ids, ",")+`)`, markArgs...); err != nil {
		return 0, 0, err
	}
	return len(events), len(toInsert), tx.Commit()
}

// listEnabledNotificationChannelsTx Gets the channels enabled in the transaction. A small number,
// no paging or caching—caching introduces an additional timing problem of when configuration
// changes take effect.
func listEnabledNotificationChannelsTx(ctx context.Context, tx *sql.Tx) ([]*NotificationChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, config, mode, filter, rate_per_min
FROM notification_channels WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NotificationAssetNames Parses the asset id into a short display name for use in push messages.
//
// The return order is consistent with the entry, and may be shorter than the entry (non-existent
// ids are skipped). Maintaining the entry order is to keep the asset order of the same vulnerable
// message stable over multiple commits—otherwise, the retry message would receive an asset order
// change and would be misread as "the asset has changed".
func (d *DB) NotificationAssetNames(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := placeholders(1, ids)
	rows, err := d.QueryContext(ctx, `SELECT id, type, domain, ip, url, app_name, bundle_id FROM assets WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[int64]string{}
	for rows.Next() {
		var (
			id                int64
			typ               string
			domain, ip, url   sql.NullString
			appName, bundleID sql.NullString
		)
		if err := rows.Scan(&id, &typ, &domain, &ip, &url, &appName, &bundleID); err != nil {
			return nil, err
		}
		labels[id] = assetDisplayName(typ, domain.String, ip.String, url.String, appName.String, bundleID.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if label, ok := labels[id]; ok && label != "" {
			out = append(out, label)
		}
	}
	return out, nil
}

// The value of the attribute is the name of the asset.
func assetDisplayName(typ, domain, ip, url, appName, bundleID string) string {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	switch typ {
	case "root_domain", "subdomain":
		return domain
	case "ip":
		return ip
	case "app":
		return pick(appName, bundleID)
	case "service", "endpoint":
		return pick(url, domain, ip)
	default:
		return pick(domain, ip, url, appName)
	}
}

// SetFindingStatusWithNotify Updates the status of the vulnerability treatment and registers a
// State Change Push event in the same transaction.
//
// Returns from=state before change; found=whether the vulnerability exists; notified=whether the
// event was successfully registered.
//
// Three intentional acts:
// - Do not log events when the state does not actually change. Repeated front-end drawer
// submissions of the same value, or automated scripts replaying the same value, should not output
// push noise.
// - If the vulnerability does not exist, returns found=false and does no write, translated by the
// caller to 404.
// - Failure to register an event does not affect the state update (see the save point description
// of RecordNotificationEventTx), so the state has been changed successfully when notified=false,
// and the caller should not report an error.
func (d *DB) SetFindingStatusWithNotify(ctx context.Context, id int64, status string) (from string, found bool, notified bool, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		from, found, _, notified, txErr = SetFindingStatusTx(ctx, tx, id, status)
		return txErr
	})
	return from, found, notified, err
}

// SetFindingStatusTx Updates the vulnerability status and registers a state change push event
// within the **caller transaction**.
//
// The pull-out transaction-level function is intended to make all paths to state changes share the
// same semantic set—previously only patchFinding went along with the version of the notification,
// and when **retest concludes that "fixed"** (the line `UPDATE findings SET status=...` in
// finding_retests) it was written directly into the library, so the channel with `on_status_change`
// is completely unable to push such a state flow: the state has changed quietly on the interface,
// and the operator must wait until the platform is opened to detect it.
//
// Returns from=state before change, found=whether the vulnerability exists, changed=whether the
// state has actually changed, and notified=whether the event was successfully registered
// (registration failure does not affect status updates, see RecordNotificationEventTx).
func SetFindingStatusTx(ctx context.Context, tx *sql.Tx, id int64, status string) (from string, found bool, changed bool, notified bool, err error) {
	var (
		vulnclass, name, severity, summary string
		taskID                             sql.NullInt64
		assetIDs                           []byte
	)
	scanErr := tx.QueryRowContext(ctx, `SELECT vulnclass, name, severity, summary, task_id, asset_ids, status
FROM findings WHERE id=$1 FOR UPDATE`, id).
		Scan(&vulnclass, &name, &severity, &summary, &taskID, &assetIDs, &from)
	if scanErr == sql.ErrNoRows {
		return "", false, false, false, nil
	}
	if scanErr != nil {
		return "", false, false, false, scanErr
	}
	found = true
	if from == status {
		// No events are logged without a real change in state: repeated commits of the same value,
		// or replays of the same power, should not generate push noise.
		return from, true, false, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		return from, true, false, false, err
	}
	var assets []int64
	_ = json.Unmarshal(assetIDs, &assets)
	notified = RecordNotificationEventTx(ctx, tx, notify.EventFindingStatusChanged, id, notify.Snapshot{
		Kind:       notify.EventFindingStatusChanged,
		FindingID:  id,
		TaskID:     taskID.Int64,
		VulnClass:  vulnclass,
		Name:       name,
		Severity:   severity,
		Summary:    summary,
		AssetIDs:   assets,
		FromStatus: from,
		ToStatus:   status,
	})
	return from, true, true, notified, nil
}

// NotificationStats is the overview count at the top of the notification page.
type NotificationStats struct {
	Channels     int   `json:"channels"`
	ChannelsOn   int   `json:"channels_on"`
	Pending      int   `json:"pending"`
	Failed       int   `json:"failed"`
	SentToday    int   `json:"sent_today"`
	BacklogAgeMS int64 `json:"backlog_age_ms"` // The oldest pending submission is milliseconds away
}

// BacklogAgeMS is the most direct indicator of whether a push is stuck—much more useful than the
// pending count, because the difference between 3 backlogs and 3 pending can be anything from 3
// seconds to 3 hours.
func (d *DB) NotificationStatsSnapshot(ctx context.Context) (*NotificationStats, error) {
	var s NotificationStats
	if err := d.QueryRowContext(ctx, `SELECT
    (SELECT count(*) FROM notification_channels),
    (SELECT count(*) FROM notification_channels WHERE enabled),
    (SELECT count(*) FROM notification_deliveries WHERE state IN ($1,$2)),
    (SELECT count(*) FROM notification_deliveries WHERE state=$3),
    (SELECT count(*) FROM notification_deliveries WHERE state=$4 AND sent_at >= date_trunc('day', now())),
    COALESCE((SELECT EXTRACT(EPOCH FROM (now() - min(created_at))) * 1000 FROM notification_deliveries WHERE state=$1), 0)::bigint`,
		NotifyStatePending, NotifyStateSending, NotifyStateFailed, NotifyStateSent).
		Scan(&s.Channels, &s.ChannelsOn, &s.Pending, &s.Failed, &s.SentToday, &s.BacklogAgeMS); err != nil {
		return nil, err
	}
	return &s, nil
}
