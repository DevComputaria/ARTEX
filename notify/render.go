package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes truncates s to no more than max bytes, ensuring that the result is valid UTF-8 and
// without truncated characters.
//
// Why you have to cut by character boundary: Markdown has a hard limit of 4096 **bytes** (not the
// number of characters), while a Chinese word is 3 bytes long. Direct byte-cuts will cut a Chinese
// character in half, outputting illegal UTF-8 — the platform side either rejects the whole line or
// displays it as random blocks.
//
// max<=0 means no limit.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max is shorter than the empty space: forgo the empty space, simply truncate, and avoid
		// results that exceed max instead.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine compresses multiple lines of text into a single line: folds all the blanks, then
// intersects by character count. Header lines for IM messages — summaries often have new lines, and
// inserting them directly into table/headers can cause layout problems. max<=0 means no length
// limit.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes truncates s to no more than max characters (not bytes), adding a space when
// exceeded. max<=0 means no limit.
//
// The difference with TruncateBytes is the platform bit rate: Telegram has a bit rate limit, while
// Telegram has a character rate limit. Using the wrong bit rate will not report an error, but will
// only make the message much shorter than expected (1 Chinese character = 3 bytes, 4096 bytes =
// about 1365 words), so both functions must be retained and used according to the channel.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML Truncates HTML fragments by character count and ensures that no truncated tags are
// generated.
//
// If you do this directly to HTML, the `<a href="htt" tag will be cut off and the platform parser
// will either reject the entire line or swallow the following body as an attribute value.
//
// Not doing tag alignment (like adding </b>): Telegram's HTML parser automatically closes unclosed
// tags, while implementing alignment itself requires handling quotes, comments, and self-closing
// tags in the attribute, with a complexity disproportionate to the benefits.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// If the tail is a fragment beginning with `<` (no `>` after the last `<`), return before `<`.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// If the tail is a truncated HTML entity (e.g., `&amp;` is cut into `&amp;`), it should also be
	// returned. Entity fragments may cause the entire message to be rejected in an entity-only
	// parser — a summary message exceeding the length limit is common and does not warrant losing
	// the entire notification for that reason.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount Calculates how many items can be **completely** placed within the budget for the
// aggregate message to be packaged as a whole item.
//
// Why truncate the entire line instead of rendering the whole text: Truncation would leave the last
// half of the entries empty, and their delivery records would still be marked as delivered—not
// visible in the message or in the delivery history, so the vulnerability is gone.
//
// Parameters: maxSize<=0 means no limit; reserve is the amount reserved for the header/tail of the
// message; size measures (platform-specific bitrates: Micro/DingTalk bytes, Telegram characters —
// using an incorrect bitrate will not report errors, only cause the Chinese message to be
// compressed to much less than the upper limit); render renders the idx entry as its actual text —
// length varies depending on the content, cannot be estimated.
//
// At least return 1 (as long as there are entries). If a single string is extremely long, this
// message should be sent and eventually cut off by the caller, otherwise an over-long string will
// permanently lock the whole set.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize are two measurements of packItemCount, named to avoid the call of a bare
// func(s string) int close package, otherwise it would be difficult to tell which one is used at a
// glance.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine Renders the list of assets as a single line of display text, skipping the remainder
// when exceeding the limit and marking the total. A vulnerability can anchor dozens of assets, and
// listing them all will overwhelm the message.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, "、")
	}
	return strings.Join(assets[:limit], "、") + " 等 " + itoa(len(assets)) + " 个"
}

// itoa is a short alias for strconv.Itoa, used only to merge display text and avoid importing
// strconv everywhere.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
