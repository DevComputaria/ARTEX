package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// This document is invariant testing:** any** error text that arises from the channel
// implementation must not carry credentials.
//
// Why pull a separate file: The initial channel use case only covers successful paths and platform
// business errors, ignoring transport layer failures altogether. And it is precisely transport
// layer errors (connection denied/DNS failed/timeout) that are the most dangerous — the *url.Error
// returned by http.Client.Do will type the **full URL** into the wrong text, whereas the
// credentials of this function are in the URL.
//
//	notification_deliveries.last_error → Clear text GET /api/notify/deliveries Response → Bypass the
//	channel configuration mask and returns to the browser Server logs → Frequently sent out Retained
//	Testing send interface 502 response → Directly fire on front end
//
// So here, instead of testing a single function, we send a failing request channel by channel,
// stating that the credential is not found in the error text.

// credentialCases covers all channel configurations where the credentials are in the URL:
// DingTalk/TingMi in query, Feishu at the end of the path, and Telegram in the middle.
var credentialCases = []struct {
	name   string
	ch     Channel
	cfg    map[string]any
	secret string
}{
	{
		name:   "DingTalk access_token in query",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send?access_token=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "WeCom key in query",
		ch:     weComChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/cgi-bin/webhook/send?key=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Feishu hook id at end of path",
		ch:     feishuChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/open-apis/bot/v2/hook/" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Telegram bot token in the middle of the path",
		ch:     telegramChannel{},
		cfg:    map[string]any{"bot_token": leakProbeToken, "chat_id": "1", "base_url": "http://127.0.0.1:1"},
		secret: leakProbeToken,
	},
	{
		name:   "DingTalk plus key",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send", "secret": leakProbeToken},
		secret: leakProbeToken,
	},
}

// leakProbeToken is a sentinel value for a credential that is unlikely to be true, used to search
// for it in the error text.
const leakProbeToken = "LEAKPROBE0123456789abcdef"

// TestChannelErrorsNeverLeakCredentials is a core invariant.
func TestChannelErrorsNeverLeakCredentials(t *testing.T) {
	for _, tc := range credentialCases {
		t.Run(tc.name, func(t *testing.T) {
			// The inevitable failed end: 127.0.0.1:1 No one listens, going the path that was
			// denied.
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{
				Items: []Item{{FindingID: 1, Severity: "high", Name: "Leak the probe."}},
			})
			if err == nil {
				t.Fatal("I'm sorry about the address.")
			}
			assertNoSecret(t, err.Error(), tc.secret)
		})
	}
}

// TestChannelErrorsNeverLeakCredentialssInPermanentPath Overrides the permanent failed branch: URL
// validation failures, platform operation errors, etc. also send out an error text, which cannot be
// authenticated either.
func TestChannelErrorsNeverLeakCredentialsInPermanentPath(t *testing.T) {
	cases := []struct {
		name string
		ch   Channel
		cfg  map[string]any
	}{
		// The URL has credentials but the format is invalid → Triggers the validateHTTPURL /
		// url.Parse branch.
		{"DingTalk address is illegal", dingTalkChannel{}, map[string]any{"webhook": "file:///" + leakProbeToken}},
		{"WeCom addresses are illegal.", weComChannel{}, map[string]any{"webhook": "gopher://" + leakProbeToken}},
		{"Feishu address is illegal.", feishuChannel{}, map[string]any{"webhook": "ftp://" + leakProbeToken + "/hook"}},
		{"Telegram API Illegal address", telegramChannel{}, map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": "file://" + leakProbeToken}},
		{"Common Webbook address is illegal", webhookChannel{}, map[string]any{"url": "javascript:" + leakProbeToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{Items: []Item{{Severity: "high"}}})
			if err == nil {
				t.Fatal("Illegal configurations should be compensated for wrong")
			}
			assertNoSecret(t, err.Error(), leakProbeToken)
		})
	}
}

func assertNoSecret(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Fatalf("Error text leaked evidence %q: \n %s", secret, text)
	}
}

func TestRedactRequestTargetKeepsOnlySchemeAndHost(t *testing.T) {
	cases := map[string]string{
		"https://oapi.dingtalk.com/robot/send?access_token=S1":    "https://oapi.dingtalk.com/…",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=S2": "https://qyapi.weixin.qq.com/…",
		"https://open.feishu.cn/open-apis/bot/v2/hook/S3":         "https://open.feishu.cn/…",
		"https://api.telegram.org/botS4/sendMessage":              "https://api.telegram.org/…",
		"http://10.0.0.5:8080/hook":                               "http://10.0.0.5:8080/…",
	}
	for in, want := range cases {
		got := redactRequestTarget(in)
		if got != want {
			t.Errorf("RedactRequest Target(%q = %q, expectations %q", in, got, want)
		}
		// The redacted result itself must no longer contain any path/query segments of the
		// original address.
		if parts := strings.SplitN(in, "://", 2); len(parts) == 2 {
			if hostAndRest := strings.SplitN(parts[1], "/", 2); len(hostAndRest) == 2 && hostAndRest[1] != "" {
				if strings.Contains(got, hostAndRest[1]) {
					t.Errorf("Redaction still contains path/ query fragments %q: %q", hostAndRest[1], got)
				}
			}
		}
	}
	// Unparsed input will never return the original string.
	for _, bad := range []string{"", "://", "not a url", "http://"} {
		if got := redactRequestTarget(bad); strings.Contains(got, bad) && bad != "" {
			t.Errorf("Unsolved input %q is recognizable %q", bad, got)
		}
	}
}

// TestRedactTransportErrorStripsURL directly tracks the specific type *url.Error: It is the return
// type of http.Client.Do, and the first scene of the leak.
func TestRedactTransportErrorStripsURL(t *testing.T) {
	inner := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	uerr := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + leakProbeToken + "/sendMessage",
		Err: inner,
	}
	got := redactTransportError(uerr)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "api.telegram.org") {
		t.Errorf("Host should be kept for checking, get %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("The bottom reason should be retained for checking, get %q", got)
	}
	// Op is also retained (POST or GET is meaningful for the query).
	if !strings.Contains(got, "Post") {
		t.Errorf("Operation name should be retained, get %q", got)
	}
}

// TestRedactURLsInTextHandlesFallback Fallback path: The addresses in custom errors other than
// *url.Error (such as errors returned by redirect policies) are also removed.
func TestRedactURLsInTextHandlesFallback(t *testing.T) {
	in := fmt.Sprintf("Reject cross-host redirects (a.example → http://b.example/bot%s/send）", leakProbeToken)
	got := redactURLsInText(in)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "http://b.example/…") {
		t.Errorf("The address should be replaced with a redaction form, get %q", got)
	}
	// Text without addresses is retained as is.
	if plain := "dial tcp: connection refused"; redactURLsInText(plain) != plain {
		t.Error("Texts without addresses should not be altered")
	}
}

// TestCrossHostRedirectRefused overrides "credentials in the URL + cross-hopping followed =
// surrender credentials".
func TestCrossHostRedirectRefused(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/robot/send?access_token="+leakProbeToken, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": redirector.URL + "/robot/send?access_token=" + leakProbeToken},
		Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("Redirection across the mainframe should be denied.")
	}
	if hit {
		t.Fatal("The jump target was visited — the evidence was released with a high degree of orientation.")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestSameHostRedirectAllowed Reverse use case: Jumps with the host (such as end-to-end slashes)
// must still be available, otherwise normal workflow will be blocked.
func TestSameHostRedirectAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robot/send" {
			// Jumping the same host, the same port.
			http.Redirect(w, r, "/robot/send/", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"},
		Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("Redirection with host should not be denied: %v", err)
	}
}
