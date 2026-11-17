package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// This document covers the "wrap-around" fix: when aggregating messages beyond the channel length
// limit, they must be truncated **around** and the number of unloaded entries reported as true, so
// that the caller only marks those that are actually delivered.
//
// The previous approach was to render the entire text and then cut it, and then the whole set of
// tags was delivered: the last half of the message disappeared empty, and the delivery history
// showed all of it was successful—the vulnerability was gone, and nowhere to be found.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 200 Chinese summaries are far more than the modest 4,096 bytes.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("Text %d byte super-ceiling %d", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("The text is not legal UTF-8")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("Only part (0 < kept < %d), obtained %d", len(m.Items), kept)
	}
	// The header must truthfully state how many articles the article contains and how many
	// remaining articles there are — otherwise the reader will assume that number in the header to
	// be the whole.
	if !strings.Contains(body, "showing") || !strings.Contains(body, "continue in the next message") {
		t.Fatalf("The head shall indicate how many more articles are not included in this article: \n %s", body[:minInt(400, len(body))])
	}
	// Only the pre-kept line should be included.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "vulnerability"+itoa(i+1)) {
			t.Fatalf("entry %d should be in this message:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "vulnerability"+itoa(kept+1)) {
		t.Fatalf("Article %d should not appear. (It belongs to the next batch)", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = no limit
	if kept != len(m.Items) {
		t.Fatalf("All should be retained without limitation of length, get kept=%d", kept)
	}
	if strings.Contains(body, "continue in the next message") {
		t.Fatalf("There should be no cut-off hint when there is no cut: \n %s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// If the budget is so small that not even one card can be installed, one must still be issued
	// (by being eventually cut off).
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("At least 1 bar should be retained, get %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("One single message should be reported for service, received %d", kept)
	}
	// The empty message has no deliverable entries.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("Empty message to 0, get %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram is limited to ** characters**; byte-by-byte compresses Chinese messages to one
	// third.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("Text %d character super-ceiling %d", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("Only part of it should be loaded, get %d", kept)
	}
	if !strings.Contains(text, "continue in the next message") {
		t.Fatalf("It should be noted that the remaining amount is not included: \n %.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("The card should only be partially loaded, get %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// Both channels do not cut off the text, and the whole batch is considered delivered.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("Precondition is not valid")
	}
	// This is confirmed indirectly by the return value of the renderer: markdownBody(0) is retained
	// when unrestricted.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("Apply all when not limiting length, get %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent is a regression test for "untrusted content must not change
// the message structure." The header and abstract are from the model output (the model reads the
// target response), and the asset name is from the URL of the target being tested.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // Must appear in the result (translational form)
		wrong []string // Not to appear in the results (untranslated form)
	}{
		{
			name: "Word break in title + outer chain",
			item: Item{
				Severity: "high",
				Name:     "Login SQL Inject \n[Emergency: Click here to verify account number] (http://attacker.tld)",
			},
			// The newline must be collapsed (otherwise it can fake a new list item/reference
			// block); square brackets and rounded brackets must be redefined (otherwise it is a
			// clickable external link).
			must:  []string{`\[Emergency: Click here to verify account number\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n [Emergency]", "\n \n [Emergency]"},
		},
		{
			name: "Picture beacon in the title",
			item: Item{
				Severity: "high",
				Name:     "Vulnerability! [](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\[\]\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "The emphasis and quotes of the Asset.",
			item: Item{
				Severity: "high",
				Name:     "Normal Title",
				Assets:   []string{"a.com/*Injection* >citation"},
			},
			must:  []string{`\*Injection\*`, `\>`},
			wrong: []string{"♪ Injected ♪"},
		},
		{
			name: "Inverse quotes and vertical lines in the summary",
			item: Item{
				Severity: "high",
				Name:     "Title",
				Summary:  "`code` *Foreword* | value",
			},
			must:  []string{"\\`code\\`", `\*Foreword\*`, `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// The writeItem of the single-bar mode is the rendering path shared by all three
			// markdown channels.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("missing escaped form %q:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("Unconverted morphology %q (could be used to inject structures or outer chains): \n %s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst Locks the turn order: backslashes must be processed first,
// otherwise a layer of backslashes will be added to the backslashes that are in the middle,
// resulting in double backslashes in the output.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("Wrong order of transition, get %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes locks a specific return: markdown escapes cannot be leaked
// into Telegram's HTML output (although they were once added in the shared header function,
// resulting in a visible backslash `\(1\)` in the Telegram message).
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "*Focus*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Telegram appears in the text a reverse slash: \n %s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
