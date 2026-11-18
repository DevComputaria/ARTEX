package notify

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskedValueHidesBodyButKeepsTailHint(t *testing.T) {
	const secret = "https://oapi.dingtalk.com/robot/send?access_token=abcdef123456"
	got := MaskedValue(secret)
	if strings.Contains(got, "abcdef123456") {
		t.Fatalf("The mask value leaks complete evidence: %q", got)
	}
	if strings.Contains(got, "oapi.dingtalk.com") {
		t.Fatalf("The mask value should not expose the address subject: %q", got)
	}
	// The last 6 digits are kept so that the user can identify which bot is.
	if !strings.HasSuffix(got, "123456") {
		t.Fatalf("The last 6 bits should be retained as identification hint: %q", got)
	}
	if !IsMasked(got) {
		t.Fatalf("The mask value must be recognized by Ismasked: %q", got)
	}
}

func TestMaskedValueShortSecretGivesNoHint(t *testing.T) {
	// If the short credential also exposes the last 6 bits, it is equivalent to exposing the entire
	// credential.
	for _, s := range []string{"abc", "abcdef", ""} {
		got := MaskedValue(s)
		if got != MaskedPrefix {
			t.Fatalf("The evidence for length %d should not give a tail hint, get %q", len(s), got)
		}
		if s != "" && strings.Contains(got, s) {
			t.Fatalf("The mask value contains the original value: %q", got)
		}
	}
}

func TestMaskConfigMasksOnlySecrets(t *testing.T) {
	cfg := map[string]any{
		"webhook": "https://example.com/hook?token=SECRETVALUE",
		"secret":  "SECtest123456",
		"port":    float64(587),
		"host":    "smtp.example.com",
	}
	masked := MaskConfig(KindDingTalk, cfg)
	for _, k := range []string{"webhook", "secret"} {
		s, _ := masked[k].(string)
		if !IsMasked(s) {
			t.Errorf("%s Should be masked, get %q", k, s)
		}
	}
	// Non-credential fields must be left as they are, otherwise the UI cannot be displayed.
	if masked["port"] != float64(587) {
		t.Errorf("Non-supported field port should not be changed: %v", masked["port"])
	}
}

func TestMaskConfigUnknownKindReturnsEmpty(t *testing.T) {
	// When the channel type is not recognized, it is preferable to have the UI display an empty
	// configuration rather than returning the original content that may contain credentials.
	got := MaskConfig("nope", map[string]any{"webhook": "https://x/y?token=LEAK"})
	if len(got) != 0 {
		t.Fatalf("Unknown channel type should return to empty configuration, get %v", got)
	}
}

func TestMaskConfigDoesNotMutateInput(t *testing.T) {
	// The mask is a display layer behavior and cannot reverse-change the true value of Curie.
	cfg := map[string]any{"webhook": "https://example.com/hook", "secret": "SECtest123456"}
	_ = MaskConfig(KindDingTalk, cfg)
	if IsMasked(cfg["secret"].(string)) {
		t.Fatal("MaskConfig modified entry, leading to real documents being overridden by masked values")
	}
}

func TestMergeConfigKeepsStoredOnMaskedIncoming(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET", "method": "POST"}
	// The user only changes the method, and the browser submits the mask value + new method.
	incoming := map[string]any{
		"webhook": MaskedValue("https://real/hook"),
		"secret":  MaskedValue("REALSECRET"),
		"method":  "PUT",
	}
	got := MergeConfig(stored, incoming)
	if got["webhook"] != "https://real/hook" || got["secret"] != "REALSECRET" {
		t.Fatalf("Mask field should retain the original library value, get %v", got)
	}
	if got["method"] != "PUT" {
		t.Fatalf("The modified field shall take effect and receive %v", got["method"])
	}
}

func TestMergeConfigEmptyStringClears(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET"}
	got := MergeConfig(stored, map[string]any{"secret": ""})
	if _, ok := got["secret"]; ok {
		t.Fatalf("Empty string should empty the field, get %v", got)
	}
	// Fields not mentioned are retained (locally updated semantics).
	if got["webhook"] != "https://real/hook" {
		t.Fatalf("Unreferenced fields should be retained, get %v", got)
	}
}

func TestMergeConfigKeepsUnmentionedStoredKeys(t *testing.T) {
	stored := map[string]any{"host": "smtp.example.com", "port": float64(587), "password": "pw"}
	got := MergeConfig(stored, map[string]any{"port": float64(465)})
	if got["host"] != "smtp.example.com" || got["password"] != "pw" {
		t.Fatalf("Unreferenced fields should be retained, get %v", got)
	}
	if got["port"] != float64(465) {
		t.Fatalf("References should be updated to %v", got["port"])
	}
}

// TestPrepareConfigUpdateBlocksDestinationSwap is the most important security invariant in this
// package: **Changing the destination address cannot carry over the old credentials**.
//
// These use cases use the exact input of the attack form (change only the address, avoiding the
// credentials), not the "correct input of the defense logic" - if only the latter is tested, the
// defense is still green.
func TestPrepareConfigUpdateBlocksDestinationSwap(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
		// wantMissing is the credential key that is expected to be called.
		wantMissing string
	}{
		{
			name: "Generic Webhook address to use the Header of Administration",
			kind: KindWebhook,
			stored: map[string]any{
				"url":     "https://legit.example.com/hook",
				"headers": map[string]any{"Authorization": "Bearer REAL-TOKEN"},
			},
			incoming:    map[string]any{"url": "https://attacker.tld/c"},
			wantMissing: "headers",
		},
		{
			name:        "Telegram Change base_url Want to send Bot Token to your endpoint",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld"},
			wantMissing: "bot_token",
		},
		{
			name:        "Mail change SMTP host wants to hand over the password.",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"host": "smtp.attacker.tld"},
			wantMissing: "password",
		},
		{
			name:        "Email off TLS must also restate the password",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "tls": false, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"tls": true},
			wantMissing: "password",
		},
		{
			// The mask value = "Keep old credentials" must also be rejected in the context of
			// address change.
			name:        "Backmail mask + new address",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld", "bot_token": MaskedValue("123456:REAL")},
			wantMissing: "bot_token",
		},
		{
			name:        "DingTalk changed Webbook wants to keep the signed key.",
			kind:        KindDingTalk,
			stored:      map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=OLD", "secret": "REALSEC"},
			incoming:    map[string]any{"webhook": "https://attacker.tld/hook"},
			wantMissing: "secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err == nil {
				t.Fatalf("Relocation without restatement of evidence should be rejected; configuration %v", merged)
			}
			var target *ErrDestinationChangedWithoutCredentials
			if !errors.As(err, &target) {
				t.Fatalf("The specific error type should be returned in order for the interface to give an actionable hint, get %T: %v", err, err)
			}
			found := false
			for _, m := range target.Missing {
				if m == tc.wantMissing {
					found = true
				}
			}
			if !found {
				t.Fatalf("Missing supporting key to name %q, get %v", tc.wantMissing, target.Missing)
			}
			// The error message should be able to guide the operator how to fix it.
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("Error message should refer to %q: %v", tc.wantMissing, err)
			}
		})
	}
}

// TestPrepareConfigUpdateAllowsLegitimateEdits Reverse use case: Normal editing cannot be
// inadvertently blocked, otherwise the protection will be bypassed or deleted as "too annoying".
func TestPrepareConfigUpdateAllowsLegitimateEdits(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
	}{
		{
			name:     "Change name only (configuration back)",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "headers": map[string]any{"Authorization": "Bearer REAL"}},
			incoming: map[string]any{"url": MaskedValue("https://legit.example.com/hook")},
		},
		{
			name:     "Just change the request method. The address doesn't change the evidence.",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "method": "POST"},
			incoming: map[string]any{"method": "PUT"},
		},
		{
			name:     "Change address and ** At the same time, give new documents",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": map[string]any{"Authorization": "Bearer NEW"}},
		},
		{
			name:     "Change address and make a clear statement that no longer requires proof.",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": ""},
		},
		{
			name:     "Telegram Change chat_id (not destination)",
			kind:     KindTelegram,
			stored:   map[string]any{"bot_token": "t", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming: map[string]any{"chat_id": "-100200"},
		},
		{
			name:     "Mail to addressee (not destination)",
			kind:     KindEmail,
			stored:   map[string]any{"host": "smtp.corp.com", "port": 587, "password": "PW", "from": "a@b.c", "to": []any{"x@y.z"}},
			incoming: map[string]any{"to": []any{"new@y.z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err != nil {
				t.Fatalf("Legitimate Edition Interrupted: %v", err)
			}
			if merged == nil {
				t.Fatal("The merger result should be returned")
			}
		})
	}
}

// TestPrepareConfigUpdatePortTypeTolerance covers a misunderstanding: The port submitted by the
// front-end is JSON number (float64), which is also read back by the Curry as float64, but the two
// values may be of different types (e.g. int vs float64). == comparison will interpret "no change"
// as "changed", and thus pop up "refill password" to users who have only changed their name — a
// false alarm that would distrust this protection.
func TestPrepareConfigUpdatePortTypeTolerance(t *testing.T) {
	stored := map[string]any{"host": "smtp.corp.com", "port": float64(587), "password": "PW"}
	// Same port, submitted in the form of an int.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 587}); err != nil {
		t.Fatalf("The same port value (different types only) should not be judged as a change of address: %v", err)
	}
	// The port must be blocked.
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 25}); err == nil {
		t.Fatal("The port changes should be stopped.")
	}
}

// TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination Overrides the "can be left empty"
// path: Telegram's base_url is left empty to indicate that it uses the official API address.
//
// Here is what would cause the channel to fail to save permanently from the second save:
//
//	The first time you save the library, MergeConfig clears the empty string as explicit, deletes
//	the key, and then saves it again. Incoming is still "", but stored has no key, and is judged to
//	be "address changed".
//
// The user does not change anything, but he cannot save it again unless he re-pastes the Bot Token.
func TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination(t *testing.T) {
	stored := map[string]any{"bot_token": "123:ABC", "chat_id": "-100", "base_url": ""}

	// The front-end buildConfig() submits a value for each field definition in the channel: an
	// empty text box submits an empty string based on the return mask.
	submit := func() map[string]any {
		return map[string]any{
			"bot_token": MaskedValue("123:ABC"),
			"chat_id":   "-100",
			"base_url":  "",
		}
	}

	// First save: Only change the channel name, config returns as it is.
	merged, err := PrepareConfigUpdate(KindTelegram, stored, submit())
	if err != nil {
		t.Fatalf("First time saved by an error: %v", err)
	}
	if _, ok := merged["base_url"]; ok {
		t.Fatal("The premise has changed: the empty string should be deleted by MergeConfig — this use case overrides the \"key disappeared\" step.")
	}

	// Second save: The submission is exactly the same as the previous one, with no changes by the
	// user.
	merged2, err := PrepareConfigUpdate(KindTelegram, merged, submit())
	if err != nil {
		t.Fatalf("Second save was accidentally blocked (user changed nothing): %v", err)
	}
	// Third, confirm that it is not “only once wrong,” but stable and safe.
	if _, err := PrepareConfigUpdate(KindTelegram, merged2, submit()); err != nil {
		t.Fatalf("Third saved by error: %v", err)
	}
	// The credentials must be retained all the way, not erased by empty string logic.
	if got := merged2["bot_token"]; got != "123:ABC" {
		t.Fatalf("Bot Token should follow the original, get %v", got)
	}
}

// TestPrepareConfigUpdateStillGuardsBlankDestinationChanges is a match for the previous use case,
// which states that an empty string is equivalent to "key does not exist" and cannot be **linked to
// any change in the real address. Both directions are real credential outbound paths — Telegram's
// Bot Token goes in the URL path, so changing base_url is equivalent to sending the Token to the
// new address.
func TestPrepareConfigUpdateStillGuardsBlankDestinationChanges(t *testing.T) {
	// Direction 1: Change from "empty" (official address) to a self-made address.
	official := map[string]any{"bot_token": "123:ABC", "chat_id": "-100"}
	if _, err := PrepareConfigUpdate(KindTelegram, official, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "https://tg-proxy.attacker.tld",
	}); err == nil {
		t.Fatal("Token has to be refilled from official address to home-made address.")
	}

	// Direction two: Clearing the custom address (= switching back to the official API) is also an
	// address change.
	proxied := map[string]any{"bot_token": "123:ABC", "base_url": "https://proxy.internal/bot"}
	if _, err := PrepareConfigUpdate(KindTelegram, proxied, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "",
	}); err == nil {
		t.Fatal("Empty home-made addresses (for official API) is also a change of address and must be refilled to Token")
	}
}

func TestDestinationKeysDeclaredForEveryKind(t *testing.T) {
	// Similar to SecretKeys: if a channel forgets to declare the destination key,
	// PrepareConfigUpdate cannot protect it.
	for kind, ch := range registry {
		if len(ch.DestinationKeys()) == 0 {
			t.Errorf("Channel %s does not declare destination key, changing address with evidence protection is invalid for it.", kind)
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("Channel %s does not declare supporting key", kind)
		}
	}
}

func TestSecretKeysDeclaredForEveryKind(t *testing.T) {
	// The compiler has already forced SecretKeys to be implemented for each channel, so here again,
	// "no channels on the mask are blank" — returning a channel with an empty slice means that its
	// credentials will be explicitly returned to the browser.
	expect := map[string]bool{
		KindDingTalk: true, KindFeishu: true, KindWeCom: true,
		KindWebhook: true, KindTelegram: true, KindEmail: true,
	}
	for kind, ch := range registry {
		if !expect[kind] {
			t.Errorf("Channel %s does not register mask expectations in the test", kind)
			continue
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("Channel %s does not state any supporting field, its configuration will be explicitly recognizable", kind)
		}
	}
}

// TestPrepareConfigUpdateRejectsMaskedInContainer Override audit pointed out a flaw: When the mask
// sentinel is inserted into a **non-string** structure (such as webhook.headers is an object),
// MergeConfig only recognizes "strings with prefixes" as masks, so the word count "__masked__" is
// treated as the real header value stored in the vault—the subsequent validation fails silently and
// without any errors.
func TestPrepareConfigUpdateRejectsMaskedInContainer(t *testing.T) {
	stored := map[string]any{
		"url":     "https://legit.example.com/hook",
		"headers": map[string]any{"Authorization": "Bearer REAL"},
	}
	// The object is carried inside by a masked sentinel.
	incoming := map[string]any{
		"headers": map[string]any{"Authorization": MaskedPrefix},
	}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, incoming); err == nil {
		t.Fatal("The interior mask of the structure should be rejected (otherwise, it would have been stored literally)")
	}
	// The entire commit object (true new value) is accepted as usual.
	ok := map[string]any{"headers": map[string]any{"Authorization": "Bearer NEW"}}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, ok); err != nil {
		t.Fatalf("The regular submission of a new request should not be stopped: %v", err)
	}
}
