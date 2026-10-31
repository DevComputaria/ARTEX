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
		name:   "钉钉 access_token 在 query",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send?access_token=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "企业微信 key 在 query",
		ch:     weComChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/cgi-bin/webhook/send?key=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "飞书 hook id 在路径末段",
		ch:     feishuChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/open-apis/bot/v2/hook/" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Telegram bot token 在路径中段",
		ch:     telegramChannel{},
		cfg:    map[string]any{"bot_token": leakProbeToken, "chat_id": "1", "base_url": "http://127.0.0.1:1"},
		secret: leakProbeToken,
	},
	{
		name:   "钉钉加签密钥",
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
				Items: []Item{{FindingID: 1, Severity: "high", Name: "泄露探针"}},
			})
			if err == nil {
				t.Fatal("对不可达地址应报错")
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
		{"钉钉地址非法", dingTalkChannel{}, map[string]any{"webhook": "file:///" + leakProbeToken}},
		{"企微地址非法", weComChannel{}, map[string]any{"webhook": "gopher://" + leakProbeToken}},
		{"飞书地址非法", feishuChannel{}, map[string]any{"webhook": "ftp://" + leakProbeToken + "/hook"}},
		{"Telegram API 地址非法", telegramChannel{}, map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": "file://" + leakProbeToken}},
		{"通用 Webhook 地址非法", webhookChannel{}, map[string]any{"url": "javascript:" + leakProbeToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{Items: []Item{{Severity: "high"}}})
			if err == nil {
				t.Fatal("非法配置应报错")
			}
			assertNoSecret(t, err.Error(), leakProbeToken)
		})
	}
}

func assertNoSecret(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Fatalf("错误文本泄露了凭据 %q:\n    %s", secret, text)
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
			t.Errorf("redactRequestTarget(%q) = %q，期望 %q", in, got, want)
		}
		// The redacted result itself must no longer contain any path/query segments of the
		// original address.
		if parts := strings.SplitN(in, "://", 2); len(parts) == 2 {
			if hostAndRest := strings.SplitN(parts[1], "/", 2); len(hostAndRest) == 2 && hostAndRest[1] != "" {
				if strings.Contains(got, hostAndRest[1]) {
					t.Errorf("脱敏后仍含路径/查询片段 %q: %q", hostAndRest[1], got)
				}
			}
		}
	}
	// Unparsed input will never return the original string.
	for _, bad := range []string{"", "://", "not a url", "http://"} {
		if got := redactRequestTarget(bad); strings.Contains(got, bad) && bad != "" {
			t.Errorf("不可解析输入 %q 被回显为 %q", bad, got)
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
		t.Errorf("应保留 host 以便排查，得到 %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("应保留底层原因以便排查，得到 %q", got)
	}
	// Op is also retained (POST or GET is meaningful for the query).
	if !strings.Contains(got, "Post") {
		t.Errorf("应保留操作名，得到 %q", got)
	}
}

// TestRedactURLsInTextHandlesFallback Fallback path: The addresses in custom errors other than
// *url.Error (such as errors returned by redirect policies) are also removed.
func TestRedactURLsInTextHandlesFallback(t *testing.T) {
	in := fmt.Sprintf("拒绝跨主机重定向（a.example → http://b.example/bot%s/send）", leakProbeToken)
	got := redactURLsInText(in)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "http://b.example/…") {
		t.Errorf("应把地址替换为脱敏形态，得到 %q", got)
	}
	// Text without addresses is retained as is.
	if plain := "dial tcp: connection refused"; redactURLsInText(plain) != plain {
		t.Error("不含地址的文本不应被改动")
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
		t.Fatal("跨主机重定向应被拒绝")
	}
	if hit {
		t.Fatal("跳转目标被访问了——凭据已随重定向外泄")
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
		t.Fatalf("同主机重定向不应被拒绝: %v", err)
	}
}
