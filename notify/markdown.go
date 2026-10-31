package notify

import (
	"fmt"
	"strings"
)

// This file is a message rendering shared by the "Markdown" channels (DingTalk, WeCom). Feishu uses
// card JSON, Telegram uses HTML, and Mail uses HTML, each rendered in an adapter.

// maxAssetsShown is the maximum number of assets listed in a message. A vulnerability could anchor
// dozens of assets, and the entire column would overload the message with no informational value —
// the domains after the fourth would not be seen in IM.
const maxAssetsShown = 3

// maxSummaryRunes is how many characters the summary is compressed to. IM messages are "tips to see
// details", not reports, and the full content is in the platform.
const maxSummaryRunes = 120

// markdownReservedBytes are reserved for the header (aggregate line + level distribution + possible
// truncation hint) and tail (platform link). When packed as a whole, this is deducted from the
// budget to ensure that the tail is not truncated — once the tail is truncated, the reader does not
// even know "which batch it is and how many more rows are missing".
const markdownReservedBytes = 320

// markdownEscape translates the markdown metacharacter.
//
// Why it is necessary: The vulnerability header, summary, type, and asset display name are all from
// **untrustworthy sources** – the header and summary are from the output of the model (the model
// reads the response of the target being tested), and the url of the asset is the full URL obtained
// by scanning (a query string containing a target-controlled string).
//
//	SQL injection\n[Emergency: Click here to verify account] (http://attacker.tld)
//
// The vulnerability is rendered as a **clickable external link** in the security engineer's
// DingTalk/Feishu; and `![](http://attacker.tld/beacon)` is pulled by the client during rendering,
// informing that "this vulnerability has been seen" and revealing the reader's IP. Even without
// malicious content, the bold or quoted blocks injected can push the critical vulnerabilities below
// out of the fold.
//
// The translation set overrides the characters that change structure or produce clickable elements
// in the heading/link/emphasis/list/citation/delete line categories. `\` must be processed first,
// otherwise the backslashes that follow it will be translated again.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText compresses unreliable text into a single line and translates it for use in markdown
// body text. Translating is the other half of translating: the line break itself can fake a new
// list item or citation block, and the translating character cannot stop it.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle Returns the message title (the header bar/card title of an IM platform) with the
// content of the **untranslated original text**.
//
// This header is shared by the renderer of four contexts: markdown body, Telegram HTML, Feishu card
// plain_text, and JSON and mail themes for general Webhook. Each context has different translation
// rules (markdown translation inserts into HTML leaves a visible backslash, while inserting JSON
// contaminates data), so translation must be done by the respective output, see writeItem /
// feishuItemLines / telegramEscape.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("漏洞汇总 · 共 %d 条", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "漏洞通知"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody Renders the message body, returning the body and ** number of actual entries**.
//
// The return value kept is the number of items actually delivered in this batch, so that the caller
// only marks the previous kept item as delivered — entries that are outside the channel length
// limit must be left for the next batch, rather than being marked successful together.
//
// maxBytes<=0 indicates no limit.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// Single messages are re-broadcast even if they are too long (by being eventually cut
		// short): a piece of information about a vulnerability is better than none at all.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[在平台中查看全部](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro Renders the beginning of the summary message: time window, bar number and
// level distribution. With these, the recipient of the summary can decide whether or not the batch
// needs to be processed immediately without having to click into the platform.
//
// In contrast, it is necessary to state "how many items are in the next message" - otherwise the
// reader will assume that the number written for the message header is the total, and that the
// entries never sent after that do not exist at all on the interface.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**近 %d 分钟新增 %d 个漏洞**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**新增 %d 个漏洞**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, "（本条显示前 %d 条，其余 %d 条将在下一条消息继续）", len(items), extra)
	}
	// Only items that actually contain ** are counted in this article, ensuring that the "serious
	// 3" is consistent with the following count.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem Renders a single vulnerability entry.
//
// prefix is used to serialize the summary list; single=true renders the full version (including the
// summary and backlink), and only one line of summary is rendered in the summary list — otherwise
// 50 summaries would be a long document.
//
// All content from external sources (title/type/asset/abstract) is markdownText: single-line +
// transliteration. The return string is configured by the admin as public_base_url, not trusted
// content, and must be a pointable link, so it is output as it is.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// Aggregate mode: single-line presentation, with assets and summary compressed afterwards.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**状态变更**：%s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**类型**：%s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**资产**：%s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**摘要**：%s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[查看详情](%s)\n", it.DetailURL)
	}
}
