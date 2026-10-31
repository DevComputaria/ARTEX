package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// telegramTextLimit is the upper limit (number of characters) for the text field of Telegram
// sendMessage.
const telegramTextLimit = 4096

// telegramChannel implements the Telegram Bot API.
//
// Features of the platform:
// - The token is all in the URL path (/bot<token>/sendMessage), no signature required.
// - Use HTML parsing mode instead of MarkdownV2: MarkdownV2 requires a translation of
// `_*[]()~`>#+-=|{}.!`, which is 18 characters long, and if one is omitted the entire message is
// rejected; HTML requires only a translation of & < > three.
// - The operation error is also hidden in HTTP 200, and is judged by the ok field.
type telegramChannel struct{}

func (telegramChannel) Kind() string { return KindTelegram }

// Telegram single chats about 1 message/second, group chats 20 messages/minute.
func (telegramChannel) DefaultRatePerMin() int { return 20 }

// The Bot Token is the complete credential; chat_id is just the recipient, not secret (no Token can
// be obtained from it, nor can a message be sent).
func (telegramChannel) SecretKeys() []string { return []string{"bot_token"} }

// The base_url determines which API endpoint the Token is sent to (for example, a custom-built
// backend), and changing it requires re-statement of the Token.
func (telegramChannel) DestinationKeys() []string { return []string{"base_url"} }

func (telegramChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "bot_token") == "" {
		return errors.New("缺少 Bot Token")
	}
	if cfgString(cfg, "chat_id") == "" {
		return errors.New("缺少 Chat ID")
	}
	if base := cfgString(cfg, "base_url"); base != "" {
		if err := validateHTTPURL(base); err != nil {
			return fmt.Errorf("API 地址无效: %w", err)
		}
	}
	return nil
}

func (c telegramChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := telegramEndpoint(cfg)
	if err != nil {
		return 0, Permanent(err)
	}
	text, kept := telegramHTML(m)
	payload := map[string]any{
		"chat_id":                  cfgString(cfg, "chat_id"),
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": false,
	}
	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("解析 Telegram 响应失败: %w (%s)", err, snippet(raw))
	}
	if res.OK {
		return kept, nil
	}
	// 429 is a limiter, and retry after avoidance is valid; the rest (400 parameter error, 401
	// token error, 403 blacked out, 404 chat does not exist) are configuration issues that will not
	// heal themselves.
	if res.ErrorCode == 429 {
		return 0, fmt.Errorf("Telegram 限流: %s", res.Description)
	}
	return 0, Permanent(fmt.Errorf("Telegram 返回错误 %d: %s", res.ErrorCode, res.Description))
}

// telegramEndpoint Compile the sendMessage address. Base_url is used for the official API if left
// empty, and for custom Bot API generation (a common need in domestic networks) if not empty.
func telegramEndpoint(cfg map[string]any) (string, error) {
	base := cfgString(cfg, "base_url")
	if base == "" {
		base = "https://api.telegram.org"
	}
	base = strings.TrimSuffix(base, "/")
	token := cfgString(cfg, "bot_token")
	raw := base + "/bot" + token + "/sendMessage"
	u, err := url.Parse(raw)
	if err != nil {
		// Not transmitted err: The address contains a Bot Token, and even the addr should not be
		// returned at this time.
		return "", fmt.Errorf("拼接 API 地址失败（API 地址：%s）", redactRequestTarget(base))
	}
	return u.String(), nil
}

// telegramHTML Renders HTML body, returning the body and the number of entries actually written
// (see Channel.Send).
func telegramHTML(m Message) (string, int) {
	var b strings.Builder
	b.WriteString("<b>" + telegramEscape(markdownTitle(m)) + "</b>\n")
	if m.Batch {
		// Telegram has a maximum of **characters**, so packing is also measured by characters
		// (runeSize).
		footer := ""
		if m.HomeURL != "" {
			footer = fmt.Sprintf("\n\n<a href=\"%s\">在平台中查看全部</a>", telegramEscapeAttr(m.HomeURL))
		}
		kept := packItemCount(m.Items, telegramTextLimit, telegramReservedRunes, footer, runeSize, func(it Item, idx int) string {
			return telegramBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		b.Reset()
		b.WriteString("<b>" + telegramEscape(telegramBatchTitle(m, items, len(m.Items))) + "</b>")
		for i, it := range items {
			b.WriteString("\n" + telegramEscape(telegramBatchLine(it, i+1)))
		}
		b.WriteString(footer)
		return TruncateHTML(b.String(), telegramTextLimit), kept
	}
	if len(m.Items) == 0 {
		return b.String(), 0
	}
	it := m.Items[0]
	if it.IsStatusChange() {
		b.WriteString(fmt.Sprintf("\n<b>状态变更</b>：%s → %s",
			telegramEscape(StatusLabel(it.FromStatus)), telegramEscape(StatusLabel(it.ToStatus))))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		b.WriteString("\n<b>类型</b>：" + telegramEscape(it.VulnClass))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		b.WriteString("\n<b>资产</b>：" + telegramEscape(a))
	}
	if s := OneLine(it.Summary, maxSummaryRunes); s != "" {
		b.WriteString("\n<b>摘要</b>：" + telegramEscape(s))
	}
	if it.DetailURL != "" {
		b.WriteString(fmt.Sprintf("\n\n<a href=\"%s\">查看详情</a>", telegramEscapeAttr(it.DetailURL)))
	}
	return TruncateHTML(b.String(), telegramTextLimit), 1
}

// telegramReservedRunes Reserved for the message header and any truncation hints that may appear
// (in characters).
const telegramReservedRunes = 160

// telegramBatchLine A line in the render summary (not translated, unified by the caller).
func telegramBatchLine(it Item, idx int) string {
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		return fmt.Sprintf("%d. %s · %s — %s", idx, SeverityLabel(it.Severity), it.Title(), a)
	}
	return fmt.Sprintf("%d. %s · %s", idx, SeverityLabel(it.Severity), it.Title())
}

// The number of rows is the number of rows that **this article actually contains**, not the total
// number of rows in the batch — otherwise the reader would think that the number written for the
// message header is the total number.
func telegramBatchTitle(m Message, items []Item, total int) string {
	title := fmt.Sprintf("漏洞汇总 · 共 %d 条", total)
	if extra := total - len(items); extra > 0 {
		title += fmt.Sprintf("（显示前 %d 条，其余 %d 条下一条继续）", len(items), extra)
	}
	if m.WindowMinutes > 0 {
		title = fmt.Sprintf("近 %d 分钟 · %s", m.WindowMinutes, title)
	}
	return title
}

// Telegram only recognizes these three entities, and the entities that already exist, such as
// &amp;, are escaped a second time after being translated — this is exactly what we expect: we want
// to display the original characters, not have the user inject HTML.
func telegramEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// telegramEscapeAttr Transposes the HTML attribute value. In addition to transposing text, it also
// handles quotation marks — quotation marks in URLs will pre-close the href attribute, turning the
// content after them into an injection point.
func telegramEscapeAttr(s string) string {
	s = telegramEscape(s)
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
