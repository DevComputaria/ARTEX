package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit is the hard limit (bytes, non-characters) for markdown content in weCombots.
// This is the tightest limit of all six channels and is the main reason for the existence of
// TruncateBytes.
const weComMarkdownLimit = 4096

// weComChannel Implements WeCom group robots.
//
// Features of the platform:
// - Only authentication via key on the URL, no signature supported — so the webhook address itself
// is the entire credential.
// - markdown content limit 4096 ** bytes**, over-long whole string rejected (not truncated).
// - 20 messages per minute, also with client limits.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// WeCom only has one Webhook credential (a key on the URL), and it does not support checksums—the
// entire address is the entire credential, no other fields need to be masked.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// The only Webhook field is the one that is both the destination and the credential, so there is no
// "remained credential after re-addressing".
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("缺少 Webhook 地址")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 地址无效: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// The aggregate batch can be very long (50 entries × one line per entry + prefix), 4096 bytes,
	// and easily exceeded. The truncation is done here rather than on the platform: rejection means
	// that the whole batch is lost, and the truncation reaches at least the first few entries.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("解析企业微信响应失败: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 is an interface call over the limit—the platform's rate_per_min window will scroll,
		// and retry after avoidance is effective, so it is explicitly classified as retryable.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("企业微信限流 %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 is a webhook key invalid — permanently failed, retry will not heal.
		return 0, Permanent(fmt.Errorf("企业微信返回错误 %d: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
