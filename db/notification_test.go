package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// These SQLs use FOR UPDATE SKIP LOCKED, make_interval, JSONB, multiline IN(...) placeholders, and
// are all written in a "compile-pass but runtime error" style that must be run to be verified.

func notifyTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newTestChannel Creates a channel that is automatically deleted at the end of the test.
func newTestChannel(t *testing.T, d *DB, kind, mode string, filter string) *NotificationChannel {
	t.Helper()
	if filter == "" {
		filter = `{}`
	}
	ch := &NotificationChannel{
		Name:       "Test Channels -" + t.Name(),
		Kind:       kind,
		Mode:       mode,
		Config:     json.RawMessage(`{"webhook":"https://example.com/hook"}`),
		Filter:     json.RawMessage(filter),
		RatePerMin: 100,
	}
	id, err := d.SaveNotificationChannel(context.Background(), ch)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	ch.ID = id
	return ch
}

// addTestEvent writes an event directly (without finding) for testing assignment and delivery.
func addTestEvent(t *testing.T, d *DB, kind string, findingID int64, snap notify.Snapshot) int64 {
	t.Helper()
	snap.Kind = kind
	snap.FindingID = findingID
	id, err := d.AddNotificationEvent(context.Background(), kind, findingID, snap)
	if err != nil {
		t.Fatalf("Writing event failed: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE id=$1`, id) })
	return id
}

func TestNotificationAssetNamesResolvesAndPreservesOrder(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// There are three types of assets with their own display channels: domain name, IP, URL.
	insertAsset := func(query, value string) int64 {
		t.Helper()
		var id int64
		if err := d.QueryRow(query, value).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	domID := insertAsset(`INSERT INTO assets(type, domain) VALUES('subdomain',$1) RETURNING id`, "a.example.com")
	ipID := insertAsset(`INSERT INTO assets(type, ip) VALUES('ip',$1) RETURNING id`, "10.1.2.3")
	svcID := insertAsset(`INSERT INTO assets(type, url) VALUES('service',$1) RETURNING id`, "https://a.example.com/admin")
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id IN ($1,$2,$3)`, domID, ipID, svcID)
	})

	// The incoming sequence is deliberately misplaced and contains an id that does not exist.
	got, err := d.NotificationAssetNames(ctx, []int64{svcID, 999999999, domID, ipID, svcID})
	if err != nil {
		t.Fatalf("parse asset name: %v", err)
	}
	want := []string{"https://a.example.com/admin", "a.example.com", "10.1.2.3"}
	if len(got) != len(want) {
		t.Fatalf("The number of asset names does not match, expected %v gets %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("The order/take does not match, expected %v gets %v", want, got)
		}
	}
}

// TestRecordNotificationEventTxUnwindsOnFailure is a core use case of the savepoint mechanism: In
// the transaction, let notification_events fail (temporarily with a constant false constraint), and
// assert that ① the function reports false ② the transaction does not enter an aborted state and
// subsequent statements can still be executed.
//
// Without a save point, PostgreSQL aborts the entire transaction and any subsequent statements fail
// with "current transaction is aborted" — which is the fault path for "a problem with a
// notification table resulting in a vulnerability not being saved to the library".
//
// The SQL statement is a static string, and the SQL statement is not a dynamic string.
func TestRecordNotificationEventTxUnwindsOnFailure(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// Defensive cleanup: If the history run has left this constraint, remove it first.
	if _, err := d.Exec(`ALTER TABLE notification_events DROP CONSTRAINT IF EXISTS notify_test_never`); err != nil {
		t.Fatal(err)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // Removes temporary constraints, see function note

	// NOT VALID: Only binds the next row, not checking for historical events already in the store
	// (otherwise, the inventory row violation would result in the constraint not being added).
	if _, err := tx.ExecContext(ctx, `ALTER TABLE notification_events ADD CONSTRAINT notify_test_never CHECK (false) NOT VALID`); err != nil {
		t.Fatalf("Failed to add temporary restraint: %v", err)
	}
	if RecordNotificationEventTx(ctx, tx, notify.EventFindingCreated, 1, notify.Snapshot{Severity: "high"}) {
		t.Fatal("Report success if bound to inevitable failure")
	}
	// Key statement: Business is still working.
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("The matter has been contaminated (the point of preservation is not in effect): %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}
	// Confirm that the DDL has been revoked by rollback, leaving no mine for subsequent use cases.
	var exists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname='notify_test_never')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("The temporary restraint has not been rolled back. It will pollute the following example.")
	}
}

func TestFanOutRoutesEventsByFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	all := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	onlyCritical := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"min_severity":"critical"}`)
	sqlOnly := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["SQL"]}`)

	highSQL := addTestEvent(t, d, notify.EventFindingCreated, 1001, notify.Snapshot{Severity: "high", VulnClass: "SQL Injection"})
	lowXSS := addTestEvent(t, d, notify.EventFindingCreated, 1002, notify.Snapshot{Severity: "low", VulnClass: "XSS"})
	criticalXSS := addTestEvent(t, d, notify.EventFindingCreated, 1003, notify.Snapshot{Severity: "critical", VulnClass: "XSS"})

	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	cases := []struct {
		name    string
		eventID int64
		channel int64
		want    bool
	}{
		{"All received channels.", highSQL, all.ID, true},
		{"Copy that, low.", lowXSS, all.ID, true},
		{"Only serious channels skip high", highSQL, onlyCritical.ID, false},
		{"Only serious sources received, critical.", criticalXSS, onlyCritical.ID, true},
		{"SQL Only received through SQL", highSQL, sqlOnly.ID, true},
		{"SQL Skip XSS only", lowXSS, sqlOnly.ID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var exists bool
			if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notification_deliveries WHERE event_id=$1 AND channel_id=$2)`,
				tc.eventID, tc.channel).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != tc.want {
				t.Fatalf("Is there: Expected %v Get %v", tc.want, exists)
			}
		})
	}

	// Re-assignation should not result in a duplicate submission (fanned_out potentiation).
	events, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if events != 0 || deliveries != 0 {
		t.Fatalf("Assigned events should not be reprocessed, and are given events=%d deliveries=%d", events, deliveries)
	}
}

// TestFanOutMarksEventsWithNoMatchingChannel covers the case where "the event does not match any
// channel." Such an event must be marked as assigned anyway, otherwise it will remain in the
// pending collection forever and be swept every tick.
func TestFanOutMarksEventsWithNoMatchingChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	pick := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["never matching type"]}`)
	_ = pick

	ev := addTestEvent(t, d, notify.EventFindingCreated, 2001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	_, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("Shouldn't be a delivery, get %d", deliveries)
	}
	var fanned bool
	if err := d.QueryRowContext(ctx, `SELECT fanned_out FROM notification_events WHERE id=$1`, ev).Scan(&fanned); err != nil {
		t.Fatal(err)
	}
	if !fanned {
		t.Fatal("The missed channel must also be marked as assigned, otherwise it will be wiped out indefinitely.")
	}
}

func TestClaimRealtimeDeliveriesHonorsLeaseAndMode(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	realtime := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	digest := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)

	addTestEvent(t, d, notify.EventFindingCreated, 3001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// The realtime reception should only get the realtime channel, not the digest channel.
	got, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatalf("Fetching failed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Should get one, get %d", len(got))
	}
	if got[0].State != NotifyStateSending || got[0].Attempts != 1 {
		t.Fatalf("If the request is sending and attempts=1, we get state=%s attempts = %d", got[0].State, got[0].Attempts)
	}
	// The rendering context for the associated load must be complete (channel configuration + event
	// snapshot + finding id).
	if got[0].Channel == nil || len(got[0].Channel.Config) == 0 {
		t.Fatal("There's no channel configuration for the results. Rendering will fail.")
	}
	if got[0].FindingID != 3001 {
		t.Fatalf("Finding id not brought out of event, get %d", got[0].FindingID)
	}

	// The lease is not expired, and the second pick-up should be empty—this is an assurance that
	// "the same line will not be delivered by two dispatchers at the same time."
	again, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("Non-receipt during lease period, obtain %d", len(again))
	}

	// The delivery of the digest channel should not be affected by real-time pickups.
	left, err := d.ClaimRealtimeDeliveries(ctx, digest.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("Real-time receipt of a delivery that should not be obtained from the digest channel, get %d", len(left))
	}
}

// TestClaimExpiredLeaseRecovers Covers crashes that heal themselves: a process that hangs on the
// delivery path leaves a sending line, which must be reclaimed after the lease expires, otherwise
// the delivery is stuck forever.
func TestClaimExpiredLeaseRecovers(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 4001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	first, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("First recovery failed: %v (Article %d)", err, len(first))
	}
	// Manually push the lease back to simulate "lease expired."
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE id=$1`, first[0].ID); err != nil {
		t.Fatal(err)
	}
	second, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("expired sending row should be reclaimable; got %d entries", len(second))
	}
	if second[0].Attempts != 2 {
		t.Fatalf("Recapitulation of add-up attempts, get %d", second[0].Attempts)
	}
}

func TestClaimSkipsDisabledChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 5001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// Disable will mark the inventory to be sent together as skipped.
	if err := d.SetNotificationChannelEnabled(ctx, ch.ID, false); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateSkipped {
		t.Fatalf("Deactivated channel stock to be delivered should be marked as skipped, get %s", state)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("Disabled channels should not be available, get %d", len(got))
	}
}

func TestDigestBatchDueAndStableBatchID(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 3; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(6000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// Batch should not expire under a cycle of 0.30 minutes when newly built and 0.30 minutes old.
	due, err := d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatalf("check batch expiry: %v", err)
	}
	if due {
		t.Fatal("The newly created batch should not expire immediately")
	}

	// Age the creation times of the three submissions together to simulate a batch with a
	// stockpiling cycle.
	if _, err := d.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '40 minutes' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	due, err = d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("Batch beyond cycle shall be deemed to have expired")
	}

	batch, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatalf("Retrieves failed: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("The aggregation shall take all three at once. Get %d", len(batch))
	}
	if batch[0].BatchID == nil {
		t.Fatal("Consolidated batch must be written batch_id, otherwise it's not clear from history that these items were sent together.")
	}
	firstBatchID := *batch[0].BatchID
	for _, dl := range batch {
		if dl.BatchID == nil || *dl.BatchID != firstBatchID {
			t.Fatalf("Same batch should be shared batch_id, get %v vs %d", dl.BatchID, firstBatchID)
		}
	}

	// Let the batch fail and reorder it, but batch_id must be kept at its original value (the role
	// of COALESCE): otherwise one retry will erase the fact that "this batch was sent together".
	//
	// The whole batch must be rearranged, not just one — as is done when the delivery engine sends
	// a summary message (one message representing the whole batch, both successful and
	// unsuccessful). If only one message is rearranged, the rest will naturally get that one
	// message back during the lease period.
	allIDs := make([]int64, 0, len(batch))
	for _, dl := range batch {
		allIDs = append(allIDs, dl.ID)
	}
	if err := d.RescheduleDeliveries(ctx, allIDs, time.Second, "Simulation Failed"); err != nil {
		t.Fatal(err)
	}
	// The time has come to put leases behind us and to simulate a retreat.
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 3 {
		t.Fatalf("Recapitulation should have all 3 bars, get %d", len(reclaimed))
	}
	if reclaimed[0].BatchID == nil || *reclaimed[0].BatchID != firstBatchID {
		t.Fatalf("Retry after batch_id should keep the original value %d, get %v", firstBatchID, reclaimed[0].BatchID)
	}
}

func TestDeliveryStateTransitions(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 7001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("Received failed: %v (%d)", err, len(got))
	}
	id := got[0].ID

	if err := d.RescheduleDeliveries(ctx, []int64{id}, time.Second, "Network Dithering"); err != nil {
		t.Fatal(err)
	}
	var state, lastErr string
	if err := d.QueryRow(`SELECT state, last_error FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || lastErr != "Network Dithering" {
		t.Fatalf("Rehearsal should be followed by pending and recording the reason for getting state=%s err= %q", state, lastErr)
	}

	if err := d.FailDeliveries(ctx, []int64{id}, "I'm running out."); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateFailed {
		t.Fatalf("For failed, get %s", state)
	}

	// Manual re-issues must be reset and expire immediately, otherwise they will inherit the old
	// failed budget.
	if err := d.RetryNotificationDelivery(ctx, id); err != nil {
		t.Fatalf("Repeat failed: %v", err)
	}
	var attempts int
	var next time.Time
	if err := d.QueryRow(`SELECT state, attempts, next_attempt_at FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &attempts, &next); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || attempts != 0 {
		t.Fatalf("Resend should be pending and attempts=0, get state=%s attempts = %d", state, attempts)
	}
	if next.After(time.Now().Add(time.Second)) {
		t.Fatal("It'll be ready immediately.")
	}

	// A delivered submission should not be re-dispatched.
	if err := d.MarkDeliveriesSent(ctx, []int64{id}); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryNotificationDelivery(ctx, id); err == nil {
		t.Fatal("The delivery that has been delivered should not be allowed to re-remit.")
	}
}

func TestListNotificationDeliveriesPagingAndFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	for i := 0; i < 5; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(8000+i), notify.Snapshot{Severity: "high", Name: "Page Break Test"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute); err != nil {
		t.Fatal(err)
	}

	page1, total, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 1, 2)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	if total != 5 {
		t.Fatalf("Total should read 5, get %d", total)
	}
	if len(page1) != 2 {
		t.Fatalf("2 bars per page, get %d", len(page1))
	}
	// New in front: The first page's first id should be larger than the second page's first id.
	page2, _, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID >= page1[0].ID {
		t.Fatalf("Page order should read new before page 1 [0] = %d page 2 [0 ] =%d", page1[0].ID, page2[0].ID)
	}
	// The rendering context must be returned with the history, otherwise the list cannot show "what
	// is being pushed".
	if page1[0].ChannelName == "" || page1[0].FindingID == 0 {
		t.Fatalf("History entry missing display field: %+v", page1[0])
	}

	// Filter by status: No pending.
	pending, totalPending, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStatePending}, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if totalPending != 0 || len(pending) != 0 {
		t.Fatalf("Shouldn't be sending, get %d (total=%d)", len(pending), totalPending)
	}
}

func TestSetFindingStatusWithNotifyOnlyEmitsOnRealChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("Notification status change test", "Objective", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL Injection", Name: "State Change Cases",
		Severity: "high", Summary: "Summary",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// A finding_created event is registered when the store is dropped, and it is counted as a
	// baseline.
	var base int
	if err := d.QueryRow(`SELECT count(*) FROM notification_events WHERE finding_id=$1`, f.FindingID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base < 1 {
		t.Fatal("Vulnerability should register a pushout in the same business.")
	}

	// Change to the same state: no event should be generated (avoid duplicate submission printout
	// push noise).
	from, found, notified, err := d.SetFindingStatusWithNotify(ctx, f.FindingID, "pending")
	if err != nil || !found {
		t.Fatalf("State setup failed: found=%v err= %v", found, err)
	}
	if notified {
		t.Fatal("No pushout should be registered if the state remains unchanged")
	}
	if from != "pending" {
		t.Fatalf("Should return to pre-change status feding, get %q", from)
	}

	// Real change: The event should be recorded and from/to recorded.
	from, found, notified, err = d.SetFindingStatusWithNotify(ctx, f.FindingID, "fixed")
	if err != nil || !found {
		t.Fatalf("State setup failed: found=%v err= %v", found, err)
	}
	if !notified {
		t.Fatal("Registration of the delivery event when physical state changes")
	}
	if from != "pending" {
		t.Fatalf("From sending, get %q", from)
	}
	var snapshot []byte
	if err := d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot); err != nil {
		t.Fatalf("No state change event found: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != "fixed" {
		t.Fatalf("The state in the snapshot is not running right: %s ## %s", snap.FromStatus, snap.ToStatus)
	}
	// The snapshot must contain the required fields for rendering, otherwise the state change
	// message will be an empty shell.
	if snap.VulnClass != "SQL Injection" || snap.Severity != "high" || snap.Name != "State Change Cases" {
		t.Fatalf("Rendering field: %+v", snap)
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "fixed" {
		t.Fatalf("State should be updated to match, get %s", status)
	}

	// Not existing vulnerability: found=false, no error.
	if _, found, _, err := d.SetFindingStatusWithNotify(ctx, 999999999, "fixed"); err != nil || found {
		t.Fatalf("A non-existent vulnerability should return found=false and no error, giving found=%v err=%v", found, err)
	}
}

func TestNotificationStatsSnapshot(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 9001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	stats, err := d.NotificationStatsSnapshot(ctx)
	if err != nil {
		t.Fatalf("Statistics failed: %v", err)
	}
	if stats.Channels < 1 || stats.ChannelsOn < 1 {
		t.Fatalf("Channel count wrong: %+v", stats)
	}
	if stats.Pending < 1 {
		t.Fatalf("To be counted to be delivered: %+v", stats)
	}
	// The age of the backlog of newly created submissions should be close to zero, not negative or
	// huge.
	if stats.BacklogAgeMS < 0 || stats.BacklogAgeMS > int64(time.Hour/time.Millisecond) {
		t.Fatalf("The backlog age is illegal: %d ms", stats.BacklogAgeMS)
	}
	_ = ch
}

func TestNotificationChannelCRUDRoundTrip(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	ch := &NotificationChannel{
		Name:       "CRUD back and forth",
		Kind:       notify.KindEmail,
		Mode:       NotifyModeDigest,
		Config:     json.RawMessage(`{"host":"smtp.example.com","port":587,"from":"a@b.c","to":["x@y.z"]}`),
		Filter:     json.RawMessage(`{"min_severity":"medium","on_status_change":true}`),
		RatePerMin: 42,
	}
	id, err := d.SaveNotificationChannel(ctx, ch)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })

	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatalf("Reading failed: %v", err)
	}
	if got.Mode != NotifyModeDigest || got.RatePerMin != 42 || got.Name != "CRUD back and forth" {
		t.Fatalf("Inconsistent field: %+v", got)
	}
	if !got.IsEnabled() {
		t.Fatal("default should be enabled")
	}
	var cfg map[string]any
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["host"] != "smtp.example.com" {
		t.Fatalf("Configure not correctly stored: %v", cfg)
	}
	var filter notify.Filter
	if err := json.Unmarshal(got.Filter, &filter); err != nil {
		t.Fatal(err)
	}
	if filter.MinSeverity != "medium" || !filter.OnStatusChange {
		t.Fatalf("Filter condition not correctly stored: %+v", filter)
	}

	// Read more about the project
	got.Name = "Changed his name."
	off := false
	got.Enabled = &off
	if _, err := d.SaveNotificationChannel(ctx, got); err != nil {
		t.Fatal(err)
	}
	after, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "Changed his name." || after.IsEnabled() {
		t.Fatalf("Update not in effect: %+v", after)
	}

	// The deleted file should report "not found" instead of silent success.
	if err := d.DeleteNotificationChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.NotificationChannelByID(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("Expect ErrNotiface ChannelNotFound, get %v", err)
	}
	if err := d.DeleteNotificationChannel(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("Duplicate deletions do not exist, get %v", err)
	}
}

// TestSaveNotificationChannelKeepsExplicitZeroRate Locks a previously mistyped position: **0 is a
// valid configuration, meaning "unlimited stream", which cannot be overridden by the db layer as
// "unspecified" as default**.
//
// Historical bug: SaveNotificationChannel wrote `if RatePerMin <= 0 { take default value }`, so the
// documentation, UI prompts, takeTokens all interpreted as "0=no limit stream", only the write
// library layer was silently changed to 20 (DingTalk/Telstra/Telegram) or 100 (Feishu) — the
// operator thought that the limit was released, actually stuck, and there was no prompt.
func TestSaveNotificationChannelKeepsExplicitZeroRate(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// Explicit 0 (unlimited flow): must be saved as is.
	unlimited := &NotificationChannel{
		Name: "♪ Not ratting ♪", Kind: notify.KindDingTalk, RatePerMin: 0,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	id, err := d.SaveNotificationChannel(ctx, unlimited)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.RatePerMin != 0 {
		t.Fatalf("Rigid 0 means non-rate limiting, must be saved and obtained %d", got.RatePerMin)
	}
	if got.Mode != NotifyModeRealtime {
		t.Fatalf("Default mode should readtime, get %s", got.Mode)
	}

	// Negative values are illegal inputs and should be rejected rather than changed to other
	// values.
	bad := &NotificationChannel{
		Name: "Negative ratting", Kind: notify.KindDingTalk, RatePerMin: -1,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	if _, err := d.SaveNotificationChannel(ctx, bad); err == nil {
		t.Fatal("Negative ratte compliance should be rejected.")
	}
}

// TestDeleteChannelCascadesDeliveries Locks the external key behavior: when a channel is deleted,
// its delivery history disappears (no configuration and no history can be read), but the event
// itself remains—it may still be referenced by other channels.
func TestDeleteChannelCascadesDeliveries(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	ev := addTestEvent(t, d, notify.EventFindingCreated, 9101, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("Precondition not established: no delivery produced")
	}
	if err := d.DeleteNotificationChannel(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("The channel is deleted and its delivery cascades are removed, with %d still in place", after)
	}
	var evExists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM notification_events WHERE id=$1)`, ev).Scan(&evExists); err != nil {
		t.Fatal(err)
	}
	if !evExists {
		t.Fatal("Deleting channels should not be associated with deleting the event itself")
	}
}

// TestClaimDigestBatchHonorsCallerLimit Covers a flaw noted by the audit: The aggregate channel
// previously completely bypassed the token bucket—allow was deducted by takeTokens but not used,
// and rate_per_min did nothing for the digest mode.
func TestClaimDigestBatchHonorsCallerLimit(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 10; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(7000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// Take limit=3: only 3 can be received, the rest remain in the curry.
	got, err := d.ClaimDigestBatch(ctx, ch.ID, 3, time.Minute)
	if err != nil {
		t.Fatalf("Fetching failed: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Only 3 strips should be taken by the caller, get %d", len(got))
	}
	// limit=0 indicates that the current round has been exhausted: no entry is eligible and no
	// error should be reported.
	if got, err := d.ClaimDigestBatch(ctx, ch.ID, 0, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("0 and no error, get %d err=%v", len(got), err)
	}
}

// TestFinishFindingRetestEmitsStatusChange covers an integrity gap that the audit pointed out: When
// the retest concludes with "fixed", the state does change, but the UPDATE is written directly to
// the library, bypassing the version with notification — so the channels with on_status_change are
// completely unable to push this state flow, and the status changes silently in the interface,
// which the operators need to open the platform to know.
//
// This use case locks "all paths to state changes must register state change events."
func TestFinishFindingRetestEmitsStatusChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("Retest push test", "Objective", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL Injection", Name: "Retest target",
		Severity: "high", Summary: "Summary",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// Build a retest record and push it directly to completion.
	rt, _, _, err := d.CreateFindingRetest(ctx, f.FindingID, "Review")
	if err != nil {
		t.Fatal(err)
	}
	if rt.ConversationID == nil {
		t.Fatal("Retest should connect to a session")
	}
	// The retest must go into running before it can be concluded (in accordance with the real
	// process).
	if ok, err := d.StartFindingRetest(ctx, rt.ID); err != nil || !ok {
		t.Fatalf("Starting retest failed: ok=%v err= %v", ok, err)
	}
	if err := d.RecordFindingRetestResult(ctx, *rt.ConversationID, "fixed", "Fixed", "I'm sorry, but I don't know what to say."); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishFindingRetest(rt.ID, "completed", ""); err != nil {
		t.Fatalf("End of retest failed: %v", err)
	}

	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != FindingFixed {
		t.Fatalf("Retested state should be fixed, get %s", status)
	}

	// Key statement: There must be a state change event and from/to must be correct.
	var snapshot []byte
	err = d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2 ORDER BY id DESC LIMIT 1`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot)
	if err != nil {
		t.Fatalf("retest determines that the push event has been fixed and should be registered for state change (otherwise, the channel with on_status_change is not received): %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != FindingFixed {
		t.Fatalf("The snapshot's state flow is wrong: %s ## %s", snap.FromStatus, snap.ToStatus)
	}
	// The snapshot must have the required fields for rendering, otherwise it will be an empty
	// shell.
	if snap.Name != "Retest target" || snap.Severity != "high" {
		t.Fatalf("Rendering field: %+v", snap)
	}
}
