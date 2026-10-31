package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel Implements a custom DingTalk bot.
//
// Platform characteristics (determining the trade-offs here):
// - Single bot limit is 20 threads/minute, overflow will be silently discarded (HTTP still possible
// at 200), so limiting must be done on the client, see DefaultRatePerMin.
// - Security settings three-in-one: checkout / custom keywords / IP whitelist. Checkout is the only
// option that does not depend on the message content, so it supports only checkout (and also naked
// webhooks that do not have either of these).
// - Both success and failure return HTTP 200, distinguished by the errcode in the body — not
// checking the errcode will count a delivery failure as a success.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// The DingTalk Webhook address has an access_token, which is the credential itself, and therefore
// the entire mask.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// The target is the DingTalk webhook address itself; the change of address must be accompanied by a
// re-statement of the new address and signing key.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("缺少 Webhook 地址")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 地址无效: %w", err)
	}
	return nil
}

// Send delivers a single message. Use ActionCard (with button) if there is a back-chain and it is
// single-stringed, otherwise use markdown.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// DingTalk markdown The body of the text has no explicit byte limit, but is still protected by
	// a limit to avoid excessive expansion of the evidence field.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "查看详情",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk hides business errors in 200 responses.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("解析钉钉响应失败: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 is a signature validation failure, and 310000 is a keyword mismatch — both
		// configuration errors that will not heal themselves.
		return 0, Permanent(fmt.Errorf("钉钉返回错误 %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL Add the timestamp and sign parameters to the webhook according to the official
// signing rules.
//
// Rule: string to sign = timestamp + "\n" + secret, HMAC-SHA256 key ** is also secret**, resulting
// in base64 encoded URL.timestamp is milliseconds.secret returned as it is for the time, to support
// bots that have not enabled signing.
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// Not transmitted err: url.Parse error text contains the full address (including
		// access_token).
		return "", fmt.Errorf("解析 Webhook 地址失败: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL validates that the address is available, the protocol is supported, and the
// intranet judges the literal IP target.
//
// Two points:
//
// 1. **Error messages must be redacted**. url.Parse returns *url.Error, whose Error() includes
// **full original address**, and the function embeds credentials in these addresses (DingTalk
// access_token, WeCom key, Telegram bot token, and Feishu hook ID).
//
// 2. **Literal IP directly dials the internal network**, leaving the domain name for the dial-up
// phase (blockInternalDial is the final effective point and can also override DNS rebinding). This
// is done once in order to be prompted when saving the configuration, rather than waiting for the
// first submission failure.
//
// The restriction protocol is defensive: classes like file:///gopher:// will cause http.Client to
// behave unexpectedly (although it has been blocked by the scheme check, there is no reason to
// leave this side out).
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("地址无法解析（%s）", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("只支持 http/https，收到 %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("缺少主机名")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("拒绝投递到本机/链路本地地址 %s（如确需投递到本机服务，设置 %s=1）", ip, AllowLocalTargetsEnv)
	}
	return nil
}
