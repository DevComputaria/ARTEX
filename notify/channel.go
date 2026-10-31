package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel is an adapter for notification channels. The implementation must be **stateless**: the
// same instance is configured and reused by multiple channels, with all credentials passed from the
// cfg parameter.
type Channel interface {
	// Kind Returns the channel type identifier, which must match the registry key.
	Kind() string
	// Validate is called when saving the configuration, and checks the required fields and
	// formatting. The error returned is displayed directly to the configurator, so the text should
	// specify "which field is missing" rather than a general "configuration is invalid".
	Validate(cfg map[string]any) error
	// Send: Passes a single message, returning ** the number of actual delivered entries** and
	// errors.
	//
	// Why keeps are returned: There is a message length limit on all platforms, and if the batch
	// cannot be loaded, it will be truncated. If the caller unconditionally marks the batch as
	// delivered, the truncated entries disappear—they are not visible in the message, shown as
	// successful in the delivery history, and nowhere to find that the vulnerability was never
	// sent.
	//
	// The returned error indicates a submission failure, where *PermanentError indicates that no
	// retry should be made.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin Returns the officially recommended maximum rate per minute for this
	// channel, as the default rate limit when creating a new channel instance.
	DefaultRatePerMin() int
	// SecretKeys returns the key names of the credentials in the channel configuration. The values
	// of these keys are masked when the API returns, and the mask values are retained in the
	// library when they are updated.
	SecretKeys() []string
	// DestinationKeys Returns the key name in the channel configuration that determines where the
	// message is sent.
	//
	// Like SecretKeys, this is a security-related thing: the target address and credentials are two
	// separate sets of fields, and if you allow "change only the address, keep the credentials as
	// they are", anyone who can change the channel configuration can send the true credentials of
	// the curry to the server they control, and the channel configuration mask is completely
	// meaningless. See PrepareConfigUpdate for more details.
	DestinationKeys() []string
}

// The registry is the channel registry. It is intentionally registered with explicit size rather
// than init(): this way "what channels are there" can be seen in one place, and newly added
// channels will be exposed to omissions at compile time rather than by runtime side effects.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get is implemented by channel type.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind reports whether kind is a supported channel type.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds Returns all supported channel types, ordered by dictionary (for UI dropdown stable
// display).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError marks a delivery failure that should not be retryed: credential error, target
// denied, request body illegal, etc. Retrying is only useful for temporary failures (network
// instability, rate limiting, or endpoint 5xx); permanent failures are not retried repeatedly
// will fail and will wipe the real error out of the retry log.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent Marks err as a permanent failure. Returns nil when err is nil, which is conveniently
// written as `return Permanent(someCheck())`.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether there is a permanent failure flag on the err string.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- Configuring the read helper ----
//
// The channel is configured from the JSONB column from the database, which is reverse-serialized by
// encoding/json to map[string]any, with float64 values and an array of []any.

// cfgString Takes the string configuration, cutting out all pre- and post-spaces — copying and
// pasting from a web form is easy.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt Gets an integer configuration, compatible with both float64 (JSON default) and string
// sources.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool Gets a Boolean configuration, compatible with the string "true"/"1".
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings Takes the string array configuration, automatically trimming and discarding empty
// strings.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// Single strings are also accepted, which facilitates the submission of forms with only one
		// value.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap Gets a string mapping configuration item (such as a custom HTTP header), blanking all key
// values and discarding the blank key.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
