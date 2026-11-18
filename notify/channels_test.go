package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// singleMsg constructs a single message with quotation marks and a new line. Intentionally use
// headers/abstracts containing `"` and `\n`: This is the most common input for unauthorized JSON
// output by template insertions.
func singleMsg() Message {
	return Message{
		Items: []Item{{
			FindingID: 42,
			Name:      `"SQL Injection" risk`,
			VulnClass: "SQL Injection",
			Severity:  "high",
			Summary:   "Parameter id \n is not filtered resulting in injection",
			Assets:    []string{"a.example.com", "b.example.com"},
			DetailURL: "https://artex.local/function/findings/detail?id=42",
		}},
	}
}

// batchMsg builds a batch of summary messages.
func batchMsg(n int) Message {
	m := Message{Batch: true, WindowMinutes: 30, HomeURL: "https://artex.local/function/findings"}
	for i := 0; i < n; i++ {
		m.Items = append(m.Items, Item{
			FindingID: int64(i + 1),
			Name:      "vulnerability" + itoa(i+1),
			VulnClass: "XSS",
			Severity:  "medium",
			Summary:   "reflective stand-by script",
			Assets:    []string{"target.example.com"},
		})
	}
	return m
}

// capturePost sets up a fake receiver, which returns the received request body and header to the
// assertion function.
func capturePost(t *testing.T, respBody string, assert func(t *testing.T, body map[string]any, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("Request is not legal JSON: %v \n Original: %s", err, raw)
			}
		}
		if assert != nil {
			assert(t, body, r)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDingTalkSendsActionCardWhenLinkPresent(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "actionCard" {
			t.Fatalf("When there's a link, actioncard, get %v", body["msgtype"])
		}
		card, _ := body["actionCard"].(map[string]any)
		if card["singleURL"] != "https://artex.local/function/findings/detail?id=42" {
			t.Errorf("Return chain lost: %v", card["singleURL"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

func TestDingTalkFallsBackToMarkdownForBatch(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "markdown" {
			t.Fatalf("Summary message should be sent, copy %v", body["msgtype"])
		}
		md, _ := body["markdown"].(map[string]any)
		if !strings.Contains(md["text"].(string), "last 30 minutes") {
			t.Errorf("Summary Body Missing Time Window: %v", md["text"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, batchMsg(3)); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

// TestDingTalkBusinessErrorIsPermanent Locks the determination of "HTTP 200 but errcode not 0". Not
// checking for errcode will count a delivery failure as a success — a common mistake among domestic
// IM platforms.
func TestDingTalkBusinessErrorIsPermanent(t *testing.T) {
	srv := capturePost(t, `{"errcode":310000,"errmsg":"keywords not in content"}`, nil)
	_, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg())
	if err == nil {
		t.Fatal("Errcode Non-0 Wrong to report")
	}
	if !IsPermanent(err) {
		t.Fatalf("The keyword mismatch is a configuration error and should be marked as permanent failure, obtained %v", err)
	}
	if !strings.Contains(err.Error(), "310000") {
		t.Errorf("Error message should be loaded with platform error code, get %v", err)
	}
}

func TestWeComTruncatesCJKWithinByteLimit(t *testing.T) {
	var contentLen int
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		md, _ := body["markdown"].(map[string]any)
		content, _ := md["content"].(string)
		contentLen = len(content)
		if !utf8.ValidString(content) {
			t.Fatal("The intercept is not legal. UTF-8 — WeCom will reject the whole thing.")
		}
	})
	// Make a long enough Chinese summary, which must be longer than 4096 bytes.
	m := batchMsg(200)
	if _, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, m); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
	if contentLen > weComMarkdownLimit {
		t.Fatalf("Text %d bytes above the WeCom cap %d", contentLen, weComMarkdownLimit)
	}
	if contentLen == 0 {
		t.Fatal("Text is empty")
	}
}

func TestWeComRateLimitIsRetryableButKeyErrorIsPermanent(t *testing.T) {
	limited := capturePost(t, `{"errcode":45009,"errmsg":"api freq out of limit"}`, nil)
	_, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": limited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("45009 is rolling window ratte limiting, should retry, get %v", err)
	}

	badKey := capturePost(t, `{"errcode":93000,"errmsg":"invalid webhook url"}`, nil)
	_, err = (weComChannel{}).Send(context.Background(), map[string]any{"webhook": badKey.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("93,000 is key invalid, retry won't heal itself and should fail forever to get %v", err)
	}
}

func TestFeishuCardStructureAndSign(t *testing.T) {
	const secret = "SECtest123"
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msg_type"] != "interactive" {
			t.Fatalf("Interactive cards should be sent, get %v", body["msg_type"])
		}
		card, _ := body["card"].(map[string]any)
		header, _ := card["header"].(map[string]any)
		if header["template"] != "orange" {
			t.Errorf("High level should be colored by orange, get %v", header["template"])
		}
		// If you specify secret, you must have the option to sign it, otherwise Feishu will reject
		// it with 19021.
		if body["sign"] == nil || body["timestamp"] == nil {
			t.Fatalf("Synchronising folder failed: %v", body)
		}
		// The card element should contain a button with a url pointing to the details of the
		// vulnerability.
		elements, _ := card["elements"].([]any)
		foundButton := false
		for _, e := range elements {
			em, _ := e.(map[string]any)
			if em["tag"] != "action" {
				continue
			}
			actions, _ := em["actions"].([]any)
			for _, a := range actions {
				am, _ := a.(map[string]any)
				if am["url"] == "https://artex.local/function/findings/detail?id=42" {
					foundButton = true
				}
			}
		}
		if !foundButton {
			t.Fatal("There's no button in the card pointing to the details page.")
		}
	})
	cfg := map[string]any{"webhook": srv.URL, "secret": secret}
	if _, err := (feishuChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

func TestFeishuWithoutSecretOmitsSign(t *testing.T) {
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["sign"] != nil || body["timestamp"] != nil {
			t.Fatalf("Unconfigurated secret parameters should not be added: %v", body)
		}
	})
	if _, err := (feishuChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

func TestTelegramEscapesHTMLInUntrustedContent(t *testing.T) {
	var text string
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		text, _ = body["text"].(string)
		if body["parse_mode"] != "HTML" {
			t.Fatalf("Should use HTML parsing mode, get %v", body["parse_mode"])
		}
	})
	m := Message{Items: []Item{{
		Severity: "high",
		// The title and the abstract are from the target/model output and are not credible.
		Name:    `<script>alert(1)</script>`,
		Summary: "a & b < c",
	}}}
	if _, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": srv.URL}, m); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
	if strings.Contains(text, "<script>") {
		t.Fatalf("Untransferred HTML, presence in injection: %q", text)
	}
	if !strings.Contains(text, "&lt;script&gt;") {
		t.Fatalf("Desire to transfer entity, get %q", text)
	}
	if !strings.Contains(text, "a &amp; b") {
		t.Fatalf("& Untransferred, get %q", text)
	}
}

func TestTelegramErrorClassification(t *testing.T) {
	rateLimited := capturePost(t, `{"ok":false,"error_code":429,"description":"Too Many Requests"}`, nil)
	_, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": rateLimited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("429 Should be retry, get %v", err)
	}

	forbidden := capturePost(t, `{"ok":false,"error_code":403,"description":"bot was blocked by the user"}`, nil)
	_, err = (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": forbidden.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("403 is a configuration problem and should be permanently failed, get %v", err)
	}
}

func TestWebhookDefaultTemplateProducesValidJSON(t *testing.T) {
	// This line is the meaning of the default template: any plain `"title": "{{.Title}}"` notation
	// with quotation marks and a new line in the title will produce an invalid JSON. {{json.}} will
	// not.
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["title"] != `[High] "SQL Injection" risk` {
			t.Errorf("Title not correctly restored: %v", body["title"])
		}
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("The number of items should read 1, get %d", len(items))
		}
		it, _ := items[0].(map[string]any)
		if it["summary"] != "Parameter id \n is not filtered resulting in injection" {
			t.Errorf("Summary not correctly restored: %v", it["summary"])
		}
		// The value must be a JSON number rather than a string (notations like json:...,string"
		// will run into this problem).
		if _, ok := it["finding_id"].(float64); !ok {
			t.Errorf("finding_id should read number, get %T", it["finding_id"])
		}
	})
	if _, err := (webhookChannel{}).Send(context.Background(), map[string]any{"url": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

func TestWebhookCustomTemplateAndHeaders(t *testing.T) {
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, r *http.Request) {
		if r.Header.Get("X-Token") != "s3cret" {
			t.Errorf("Custom head loss: %v", r.Header)
		}
		if body["msg"] != "3 items" {
			t.Errorf("Custom template rendering error: %v", body["msg"])
		}
		if body["first"] != "vulnerability1" {
			t.Errorf("Renge extraction error: %v", body["first"])
		}
	})
	cfg := map[string]any{
		"url":           srv.URL,
		"headers":       map[string]any{"X-Token": "s3cret"},
		"body_template": `{"msg": {{json (printf "%d items" .Count)}}, "first":{{json (index .Items 0).Name}}}`,
	}
	if _, err := (webhookChannel{}).Send(context.Background(), cfg, batchMsg(3)); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
}

func TestWebhookRejectsNonJSONRenderResult(t *testing.T) {
	cfg := map[string]any{"url": "https://example.com/hook", "body_template": `not json at all`}
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("Rendering result non JSON should be permanent failure (template wrong, retry useless), get %v", err)
	}
}

func TestWebhookValidateCatchesBadConfigEarly(t *testing.T) {
	bad := []map[string]any{
		{},
		{"url": "file:///etc/passwd"},
		{"url": "https://example.com", "method": "DELETE"},
		{"url": "https://example.com", "body_template": `{{.Items.`},
	}
	for i, cfg := range bad {
		if err := (webhookChannel{}).Validate(cfg); err == nil {
			t.Errorf("Group configuration %d should be rejected: %v", i, cfg)
		}
	}
}

func TestEmailMessageIsWellFormed(t *testing.T) {
	msg, err := buildEmailMessage("artex@example.com", []string{"a@example.com", "b@example.com"}, singleMsg())
	if err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
	if !strings.HasPrefix(msg, "From: artex@example.com\r\n") {
		t.Fatalf("From head error: \n %s", msg)
	}
	if !strings.Contains(msg, "To: a@example.com, b@example.com\r\n") {
		t.Fatalf("To head error: \n %s", msg)
	}
	// Decode the subject whether it is plain ASCII or RFC 2047 encoded.
	if dec, err := new(mime.WordDecoder).DecodeHeader(mustExtractHeader(t, msg, "Subject")); err != nil {
		t.Fatalf("Subject could not decode: %v", err)
	} else if !strings.Contains(dec, "SQL Injection") {
		t.Fatalf("Error decoding theme: %q", dec)
	}

	// The text is base64, and the result should be valid HTML.
	parts := strings.SplitN(msg, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatal("Email missing head/body partition")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(parts[1]), "\r\n", ""))
	if err != nil {
		t.Fatalf("Body base64 decoding failed: %v", err)
	}
	html := string(decoded)
	if !strings.HasPrefix(html, "<div") {
		t.Fatalf("Text not HTML: %.80s", html)
	}
	// The title appears as it is in the text: Double quotes in HTML text content are valid
	// characters and do not need to be translated. &quot; Retained as is &quot; is asserted here to
	// prevent future mistaken translations of the quotation marks, which would make the Chinese
	// quotation mark appear as &quot;.
	if !strings.Contains(html, `"SQL Injection" risk`) {
		t.Fatalf("Quotes in the title should be retained at the text position as they were: %.200s", html)
	}
}

// TestEmailEscapesStructuralInjection Overrides the injection that is really needed to prevent the
// message body: The vulnerability header and summary are from the target and model output, and are
// unreliable content.
func TestEmailEscapesStructuralInjection(t *testing.T) {
	m := Message{
		Items: []Item{{
			Severity:  "high",
			Name:      `<script>alert(1)</script>`,
			Summary:   "a & b > c",
			DetailURL: `https://artex.local/x?a="onmouseover=alert(1)`,
		}},
	}
	html := htmlBody(m, 0)
	if strings.Contains(html, "<script>") {
		t.Fatalf("The title is not transliterated and the label may be inserted: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("Entities expected to transfer: %s", html)
	}
	if !strings.Contains(html, "a &amp; b &gt; c") {
		t.Fatalf("& and > untransferred: %s", html)
	}
	// The backchain is the public_base_url that can be assigned by the administrator, which is more
	// reliable in itself, but the location of the attribute must still be in a transliterated
	// quotation mark—otherwise a quotation marked address will be enclosed by href and injected
	// into the event handler.
	if strings.Contains(html, `onmouseover=alert(1)">`) {
		t.Fatalf("href attribute is not correctly transferred: %s", html)
	}
	if !strings.Contains(html, "&quot;") {
		t.Fatalf("The quote for the attribute position should be transposed: %s", html)
	}
}

func mustExtractHeader(t *testing.T, msg, name string) string {
	t.Helper()
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimPrefix(line, name+": ")
		}
	}
	t.Fatalf("No head found %s", name)
	return ""
}

func TestChannelValidateReportsMissingFields(t *testing.T) {
	// The validation errors are displayed directly to the configurator, who must say what is
	// missing, rather than a generic "configuration invalid".
	cases := []struct {
		kind   string
		cfg    map[string]any
		substr string
	}{
		{KindDingTalk, map[string]any{}, "webhook"},
		{KindFeishu, map[string]any{}, "webhook"},
		{KindWeCom, map[string]any{}, "webhook"},
		{KindTelegram, map[string]any{}, "Bot Token"},
		{KindTelegram, map[string]any{"bot_token": "t"}, "Chat ID"},
		{KindEmail, map[string]any{}, "SMTP"},
		{KindEmail, map[string]any{"host": "h"}, "Port"},
		{KindEmail, map[string]any{"host": "h", "port": 587, "from": "f"}, "recipient"},
	}
	for _, tc := range cases {
		ch, ok := Get(tc.kind)
		if !ok {
			t.Fatalf("Channel %s Not registered", tc.kind)
		}
		err := ch.Validate(tc.cfg)
		if err == nil {
			t.Errorf("%s Configuration %v should be verified failed", tc.kind, tc.cfg)
			continue
		}
		if !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("%s The error information should be referred to %q, get %q", tc.kind, tc.substr, err.Error())
		}
	}
}

// TestEmailSMTPErrorClassification locks the semantic distinction between SMTP 4xx/5xx. If 4xx is
// also permanently failed, a mail server that enables graylisting will fail every push on the first
// attempt — and graylisting is exactly where automatic retry should work best.
func TestEmailSMTPErrorClassification(t *testing.T) {
	cases := []struct {
		reply     string
		permanent bool
	}{
		{"450 4.7.1 Greylisting in action, please come back later", false},
		{"451 4.3.0 Temporary system failure", false},
		{"452 4.2.2 Mailbox full", false},
		{"550 5.1.1 User unknown", true},
		{"553 5.1.3 Bad address syntax", true},
		{"554 5.7.1 Relay access denied", true},
		// If you cannot get the answer code, treat it as "retryable": try more than once rather
		// than condemn a possible instantaneous failure.
		{"unexpected EOF", false},
		{"", false},
	}
	for _, tc := range cases {
		err := smtpStageError("Recipient denied", errors.New(tc.reply))
		if got := IsPermanent(err); got != tc.permanent {
			t.Errorf("Response %q: Desire permanent=%v gets %v", tc.reply, tc.permanent, got)
		}
		// Regardless of the classification, the original text is kept for user check.
		if tc.reply != "" && !strings.Contains(err.Error(), tc.reply) {
			t.Errorf("Response %q The original text was discarded: %v", tc.reply, err)
		}
	}
}

func TestRegistryCoversAllKinds(t *testing.T) {
	// Six channels are essential – one missing will disappear silently down the UI.
	want := []string{KindDingTalk, KindEmail, KindFeishu, KindTelegram, KindWebhook, KindWeCom}
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("The number of channels should read %d, get %d: %v", len(want), len(got), got)
	}
	for _, k := range want {
		if !ValidKind(k) {
			t.Errorf("Channel %s Not registered", k)
		}
		if ch, ok := Get(k); !ok || ch.Kind() != k {
			t.Errorf("Kind() of channel %s does not match the registration key", k)
		}
	}
	if ValidKind("nope") {
		t.Error("Non-registered type should not be verified")
	}
}

func TestPermanentErrorUnwrap(t *testing.T) {
	base := &permanentSentinel{}
	err := Permanent(base)
	if !IsPermanent(err) {
		t.Fatal("Should be recognised as permanent failure")
	}
	if !strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("Error message should pass through bottom: %v", err)
	}
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) must return nar")
	}
	if IsPermanent(nil) {
		t.Fatal("It's not a permanent failure.")
	}
}

type permanentSentinel struct{}

func (*permanentSentinel) Error() string { return "sentinel" }
