// Package notify implements the IM/mail push channel adaption layer that was discovered by the
// vulnerability.
//
// The channels are configured to pass through map[string]any (corresponding to the JSONB column
// notification_channels.config), and the content to be pushed is passed through Message. The
// advantage of this splitting is that signature computations, UTF-8 truncation, filter matches, and
// other really error-prone things can be removed from PostgreSQL's single test, and the host only
// needs to do the server-side arrangements.
//
// Concurrential agreement: The implementation of the channel must be **stateless**. The same
// Channel instance can be configured for multiple channels (even multiple bot instances of the same
// channel) to be reused simultaneously, all credentials are passed from the cfg parameter, and it
// is not allowed to cache things like webhook URLs into the fields of the implementation itself.
package notify

// The value is also a valid set of notification_channels.kind, and is checked by the server-side
// whitelist (similar to findings.status, without DB CHECK, for easy subsequent channel addition).
const (
	KindDingTalk = "dingtalk" // DingTalk Custom Bot
	KindFeishu   = "feishu"   // Feishu (including Lark) custom robot
	KindWeCom    = "wecom"    // WeCom group robots
	KindWebhook  = "webhook"  // Common Webhook: Custom Method/Header/JSON Template
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP Mail
)

// The type of event, corresponding to notification_events.kind.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind is the base value of kind, which is empty in the config.
const InitKind = KindDingTalk

// The severityRank maps the vulnerability level to a comparable sequence number. Unknown levels
// return 0, so any min_severity setting will keep out unknown levels — not push on doubt, to avoid
// misreporting the screen.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank Returns the rank's ordinal number; unknown ranks return 0.
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel returns the Chinese name of the severity level with emoji, used in the message
// heading and card color scheme. Unknown levels are displayed as they are, not falsified.
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 严重"
	case "high":
		return "🟠 高危"
	case "medium":
		return "🟡 中危"
	case "low":
		return "🔵 低危"
	default:
		return severity
	}
}

// StatusLabel Translates the disposal status into Chinese and is used for status change messages.
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "待处理"
	case "in_progress":
		return "处理中"
	case "confirmed":
		return "已确认"
	case "resolved":
		return "已处理"
	case "fixed":
		return "已修复"
	case "false_positive":
		return "误报"
	case "ignored":
		return "忽略"
	case "duplicate":
		return "重复"
	case "risk_accepted":
		return "风险接受"
	default:
		return status
	}
}

// AtLeast determines whether severity meets the min threshold. Min is empty for no threshold, so it
// will pass. Note that the order of unknown severity is 0, and it will be rejected by any non-empty
// min (see the severityRank note).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
