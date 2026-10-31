package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// Global settings key (exists in the settings key value table, no table needed).
const (
	// settingNotifyEnabled is the push master switch. Default on: It is used for maintenance
	// one-click stop, not the enable condition of the feature — the real enable condition is "is
	// there a channel allocated".
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL is the external access address for generating a backlink to the
	// vulnerability details (e.g. https://artex.example.com). Leave it empty if you don't want the
	// backlink button in the message. There is no reusable external address configuration in the
	// project, so add one here.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes is the period (minutes) of the aggregation mode.
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// The notifyTick is the query interval of the delivery engine. 3 seconds is the upper limit of
	// the engine's real-timeness, and is also the main source of delay between "vulnerability drop"
	// and "message arrives IM".
	notifyTick = 3 * time.Second
	// The notifyLease is the length of the lease when the delivery is received. It must be
	// significantly longer than the worst time-out for a single delivery (the HTTP client for the
	// notify packet time-outs by 15 seconds), otherwise the same line will be delivered by two
	// dispatchers at the same time.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick Limits the number of events per round of assignment to avoid the entire
	// historical backlog being rolled out into a submission task at once when the channel is first
	// enabled.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes is the default value for the aggregation cycle.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick is the maximum flow per round when the channel is not set to a
	// limit. It exists to prevent "one channel assigned to unlimited flow + scanning thousands of
	// vulnerabilities at once" from blocking the single-round drag-and-drop cycle.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick is the maximum number of ticks per channel.
	//
	// This limit is inversely proportional to the length of the lease: the row is notified of a
	// lease (notifyLease = 3 minutes) at the time of receipt, and if there are so many serial
	// deliveries in a round that it takes longer than the lease, the last few deliveries expire
	// before the lease is sent. This does not matter in a single process (Run is a single goroutine
	// running in series, tick will not re-enter), but when ** two processes are in a library**, the
	// end will take back the row with the expired lease and send it again, and will also increase
	// the attempts by two, and fail when the original process is still delivering.
	//
	// Takes: 3 minutes of lease / 30 seconds of single timeout = 6 is ** just right to fill the
	// lease**, zero balance, cannot be taken; take 5 leaves the worst 150 seconds with 30 seconds
	// left. This relationship is pinned by TestNotifyTickBudgetFitsWithinLease — changing
	// notifyLease, notifySendTimeout or any of the current values will fail the assertion.
	notifyMaxSendsPerChannelPerTick = 5
	// It also determines the value of the previous constant, which cannot be more than notifyLease,
	// see TestNotifyTickBudgetFitsWithinLease.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff is the avoidance sequence of failed retries, underscore the number of times
// attempted. 3 chances (including the first) correspond to db.MaxNotifyAttempts, both of which must
// be changed together.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// The Notifier is the delivery engine for the vulnerability push.
//
// Runs as a separate goroutine alongside the Scheduler (see server.New).Tick of intentional non-use
// of Scheduler: The push's real-time requirement (3 seconds) differs from the trigger's business
// pace, and the failure of both is not mutually exclusive—push jams should not affect agent
// triggers.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu protects buckets. A single mutually exclusive lock is sufficient for a small number of
	// channels and low competition, and it is not worth introducing a more granular structure for
	// it.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket is a single-channel token bucket.
//
// The use of token barrels instead of a sliding window that "clears after every minute" is because
// the latter has a bad boundary effect: 20 tokens at the end of the window, 20 more in the next
// moment, and for platforms 40 in a second would be limited; the token barrels are replenished at a
// constant rate to naturally avoid such sudden changes.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run loops until ctx is finished. Started once by server.New.
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step Run a round: first assign new events, then submit the tasks that are due.
//
// Any failure of a step is logged, without interrupting the loop - notification system failures are
// never escalated to process-level problems.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] 分派事件失败: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] 读取渠道失败: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// The token bucket is measured in the number of ** message lines** (equivalent to the
		// number of HTTP requests), not the number of vulnerabilities. In real-time mode, both are
		// the same (one vulnerability, one message); in aggregate mode, a whole set of
		// vulnerabilities is combined into one message, so only one token is consumed.
		//
		// Both models ask for tokens first, then collect them by quota – not in reverse order, or
		// the delivery under the flow-limiter has already used up the number of retries.
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan Returns the token consumption and batch size limit for the current round of the
// aggregate channel.
//
// The two return values are **two different scales**, which is why they are independent functions:
//
// - tokens is the number of messages. A batch of vulnerabilities synthesizes one message and sends
// one HTTP request, so it is constant to 1. rate_per_min therefore still applies to digest (this is
// the maximum number of aggregate messages per minute).
// - claimLimit is the maximum number of vulnerabilities in this set. It is limited only by the
// memory limit and does not have to do with the request budget.
//
// In order to make rate_per_min work for digests, the budget per round of requests
// (notifyMaxSendsPerChannelPerTick, derived from the lease) was passed directly as batch size. The
// result is that channels with rate_per_min=20 only fill up 1 token in a 3-second tick, so each
// digest message contains only 1 vulnerability — digest degenerates into "real-time pushes with
// summary cases", and readers receive a string of "nearly 30 minutes added 1 vulnerability" while
// db.MaxDigestBatchSize is never reached.
//
// This symptom is not easily detected in end-to-end testing (current use cases manually pass a
// sufficiently large limit to stepDigest, bypassing the quota calculation in the step), so the
// decision is pinned here directly by TestDigestTickPlanDecouplesBatchSizeFromSendBudget.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime Receives and delivers real-time tasks to a channel, one vulnerability per message.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 领取实时投递失败 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("渠道类型 %q 未注册", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// Rendering failures are local data problems, and retrying will not improve them.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest sends a single message when a batch of pending deliveries in a channel expires.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] 判断汇总批次失败 channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 领取汇总批次失败 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("渠道类型 %q 未注册", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// Otherwise, they will remain outside of the included, neither in the message nor in the failed
	// list — their status will be omitted by the subsequent batch flags when successfully sent, and
	// will always stop sending until the lease expires and is reclaimed.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "事件快照无法解析，本条漏洞无法渲染成消息"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] 标记坏快照投递失败 channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] 跳过 %d 条快照无法解析的投递 channel=%d", len(skipped), ch.ID)
	}
	// Only those messages that are included are passed to send:included[i] is strictly matched to
	// msg.Items[i], and send depends on this mapping to drop "channel returned loaded before K
	// items" on the correct delivery line.
	n.send(ctx, channel, cfg, msg, included)
}

// send Submits and flows the resulting state.
//
// The same batch of deliveries (possibly dozens in the aggregation mode) share one sending result:
// either delivered or retry the whole batch. Not retrying batch by batch — aggregating a single
// message and re-sending part of it would cause semantic confusion in the batch.
//
// The only exception is the **subsections resulting from channel length limits**: if the channel
// returns actually loads only the previous K items, then K+1 items must be left in the next batch,
// rather than being marked successful together. Otherwise, the truncated gaps are neither in the
// message nor in the failure list and disappear completely.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// A single bid cap is set to avoid a channel being stuck and dragging the entire remaining
	// round.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// The number of lines returned by the channel cannot exceed the number of inputs; if
			// this happens, it indicates that the rendering layer has made a mistake, and it is
			// better to process all deliveries and write down the problem than to mess up the log.
			log.Printf("[notify] 渠道回报送达条数 %d 超过投递数 %d channel=%s，按全部送达处理",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] 标记已送达失败 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// The message has reached the channel length limit: the remaining are immediately back
			// in queue, rescheduled by the next tick. Use DeferDeliveries instead of
			// RescheduleDeliveries — this is not a failure, and should not consume your retry
			// budget (you were already optimistic +1 when you received it, so it will be deducted).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("本条消息已达渠道长度上限，仅送达前 %d 条，其余留待下一批", delivered)); err != nil {
				log.Printf("[notify] 分段续发排队失败 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// The channel does not report errors or say how many messages were delivered. It is treated
		// as a failure (reverse avoidance), so that the message is not received repeatedly but
		// never marked off.
		err = fmt.Errorf("渠道未报告送达条数（delivered=%d）", delivered)
	}

	// Failure disposal** is decided item by item** rather than judged by the maximum number of
	// attempts in the batch.
	//
	// It used to be `if maxAttempts(deliveries) >= MaxNotifyAttempts` that killed the whole
	// critique, but the batch did not have the same number of attempts for each line: an old
	// delivery (attempts=2) that had tried twice would drag a new delivery (attempts=1) from the
	// same batch into failed — the new vulnerability lost one retry forever, contrary to the
	// original intention of "not letting old rows drag new rows down".
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] 标记失败状态出错 channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("重试 %d 次后仍失败: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] 标记失败状态出错 channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// Regrouping by delay: Only 3 regroups are avoided, and the number of regroups is naturally
	// small enough to avoid sending an UPDATE for each item (which would make a batch of 500 items
	// generate 500 round trips).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] 重排投递失败 channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] 投递失败 channel=%d kind=%s 永久失败=%d 重试耗尽=%d 待重试=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries Returns those in all that are not in keep (compared by pointer identity). Used
// to find deliveries that "failed to enter" messages - they must be explicitly handled and cannot
// be left in the gray area.
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt Gets the channel implementation and parses its configuration. Returns ok=false indicating
// that the type is not registered, and submission should fail directly rather than retry
// indefinitely.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// If the configuration fails to parse, it gives an empty map: the channel's own Validate
		// reports "which field is missing", which is more helpful than a JSON parsing error.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle Renders a single vulnerability message.
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch Renders the batch summary message. Parses snapshots line by line - if a single line
// is broken, it skips that one, not letting it drag the whole batch down.
//
// The return value of included is a **strictly one-to-one** correspondence between msg.Items and
// the i-th submission ↔ the i-th entry. This correspondence is strictly required: the caller
// decides whether the K-th submission tag has been delivered by "the channel return loaded the
// previous K items". If you skip bad snapshots here without removing the skipped submissions from
// the included, the underscore will be in the wrong place — the bad entries that should have failed
// will be marked as delivered, and the good entries will be mistakenly judged as not delivered. For
// those that are broken, those explicitly marked as failures by the caller, see stepDigest.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// Bad snapshots are not included in the message, nor are they included—it is the
			// caller's responsibility to dispose of them (explicit failure marking, not mixing in
			// the "delivered" Rimon crossover).
			log.Printf("[notify] 汇总批次中跳过无法解析的快照 delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("汇总批次 %d 条投递全部无法解析", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor Renders the snapshot of the event as a to-push entry, with the parsed asset name and
// details backlinked.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// Failure to parse the asset name should not stop the push: not being able to read the name
		// is much less serious than not receiving a notification, only one asset line less in the
		// message.
		log.Printf("[notify] 解析资产名失败 finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// The details page is routed to web/src/app/(main)/function/findings/detail/page.tsx, which
		// reads the vulnerability id from the query parameter id.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens takes up to want tokens from the channel token bucket, returning the actual number of
// tokens taken.
//
// One token = one message (one HTTP request). In real-time mode, the caller sends several messages;
// in aggregate mode, the entire cluster of vulnerabilities sends only one message, transmitting 1.
//
// The barrel capacity is the upper limit of the channel per minute, replenished at a constant rate.
// ratePerMin<=0 indicates unlimited flow, returning a limited but large enough value to prevent the
// single-cycle cycle from being dragged by infinite backlog.
//
// want This cap is necessary: without it, the barrel can only be emptied, and the caller has a cap
// on each round, so that extra tokens are both useless and disappear before the next
// replenishment—the buffered capacity is never reached, and even if "there is nothing to deliver
// this round" a deduction is made.
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// The rate is ratePerMin/60 per second, supplemented by the real time elapsed.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// Add a very small epsilon and take the whole: tokens are floating-point cumulative, so if 0.5
	// + 0.5 is filled twice, it may be 0.99999999999, and int() will be cut to 0 — mathematically,
	// a full bucket cannot take any tokens.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled Reads the master switch.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL returns the external address for the backlink, with the tail slash removed.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval Returns the aggregation interval, which is reset to the default if it is invalid
// or unconfigured.
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot Parses a snapshot of the corresponding event.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("投递 %d 的事件快照为空", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("解析投递 %d 的事件快照失败: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// The event type is based on the event behavior, and the snapshot may have been written by
		// an older version.
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
