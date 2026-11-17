package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// This is the most important invariant of the package. The minimum length is ** bytes**,
	// Chinese 3 bytes/word, and any implementation that is hard by bytes will cut the characters in
	// half, outputting illegal UTF-8 and being rejected by the platform.
	inputs := []string{
		"Chinese Test Contents",
		"Mixed content",
		"b.c test d teste",
		"🔴🟠🟡🔵", // 4-byte emoji, more obvious cut-offs
		strings.Repeat("vulnerability", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("Input %q max=%d: Output illegal UTF-8 %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("Enter %q max=%d: Result %d bytes above the upper limit", in, max, len(got))
			}
			// Content may not be changed while it is not being truncated.
			if len(in) <= max && got != in {
				t.Fatalf("Input %q max=%d: content changed without exceeding limit - > %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 should indicate no limit")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 should indicate no limit")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// max is smaller than the empty space itself, and cannot be exceeded by adding an empty space.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("Results at max=1 %q length %d excess", got, len(got))
	}
	// Normally, a comma should be used.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("Expect an ellipsis, get %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// The difference in bitrates with TruncateBytes must be retained: Telegram is
	// character-limited, and byte-bitrates will cut down Chinese messages to only one third.
	s := "One, two, three, four, five, six, seven, eight, nine."
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("Expected 5 characters, get %d (%q)", n, got)
	}
	// The same string should be significantly shorter by byte gauge.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("Bytes caliber should not produce the same number of characters as character calibres")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("First line \n \n second line \t with tabs multispace", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("All spaces should be folded, get %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("Continuous spaces should not be retained, get %q", got)
	}
	// The truncated copy must remain legible and legal.
	got = OneLine("One, two, three, four, five, six, seven, eight, nine.", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("Expected 4 characters, get %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// Directly truncate HTML will cut off such fragments as `<a href="htt"', and the platform will
	// reject the entire message.
	s := `<a href="http://www.example.com/" target="_blank">Example</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: Result %d", max, n)
		}
		// The tail cannot have an unclosed `<` (i.e., `<` appears in the last paragraph but no
		// `>`).
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("Mac=%d: tail label cut - %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("Noasset should return empty string, get %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a, b" {
		t.Fatalf("Unexceeded should be full column, get %q", got)
	}
	// The totals must be indicated when the ceiling is exceeded, otherwise the reader will not know
	// how many assets are not listed.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "3 more") {
		t.Fatalf("Total to be marked 5, get %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("An empty sequence of 0 should be blocked by any threshold")
	}
	if !AtLeast("critical", "") {
		t.Fatal("Empty threshold should be released")
	}
	if got := StatusLabel("fixed"); got != "Fixed" {
		t.Fatalf("Unknown state map, get %q", got)
	}
	// The unknown is revealed as it is, without labels.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("Unknown state should be recognizable, get %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity covers an omission that was noted in the audit: truncation avoids
// not only half-cut tags, but also the HTML entities that are truncated.
//
// After `&amp;` is cut into `&amp;`, an entity-only parser may reject the entire message - and
// oversized aggregate messages are common and costly.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// The tail must not contain any entity fragments with & but no corresponding;.
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: tail leaves physical fragments %q", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: Deform entity", max)
		}
	}
}
