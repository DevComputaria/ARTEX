package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// This document covers two related reinforcements: ① The delivery address must not be a springboard
// for the intranet / cloud metadata (SSRF) ② Address validation error messages must not be
// extracted from the credentials in the address
//
// Testing environment: This package uses a lot of httptest fake receivers on 127.0.0.1, and the
// gatekeeper will block them by default. So, in TestMain, AllowLocalTargetsEnv is uniformly
// enabled, and each of the following SSRF use cases explicitly clears it to assert that **default
// denial** is enabled.

func TestMain(m *testing.M) {
	// Let the regular use case connect to the local fake receiver; SSRF use cases will temporarily
	// empty themselves.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault is the core statement of SSRF protection: By default, a
// loopback address must be rejected by the **connection layer**.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // Shut down escape = default behavior
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("Default should not be allowed to deliver to ringback address")
	}
	if hit {
		t.Fatal("The request has been received from the server. The guards are not working.")
	}
	// The error message should be able to guide the user how to release (native SMTP relays are
	// validly configured).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("Rejection of information should indicate how to open it: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn Reverse use case: must be explicitly enabled, otherwise
// legitimate deployments such as native postfix / intranet relays will be dismissed.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("The display release should be capable of delivery: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // Cloud metadata endpoints – the main reason for this function
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped forms must be restored and then judged, otherwise bypass
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s Should be rejected", s)
		}
	}
	// RFC1918 Private network ** deliberately released**: Intranet self-constructed Mattermost/SMTP
	// relays are common legitimate uses. This statement fixes the tradeoff — if someone in the
	// future adds private network judgment, it will fail here, thus forcing a conscious decision
	// (rather than silently discarding a batch of deployments).
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s Should be released (the net is a common legitimate delivery target)", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets Prefix hint for overriding the configuration
// stage: Literal IP should be rejected when saved, rather than waiting for the first submission to
// fail.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s should be rejected at the configuration stage", raw)
		}
	}
	// The public and private IP addresses are passed as usual (the private IP is left for the
	// dial-up phase, where it is not blocked).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s Should be verified: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials is the branch that the audit pointed out was
// missing from my last round.
//
// If url.Parse fails, it returns *url.Error, and its Error() contains the full original address. In
// the previous implementation only redacted errors returned by http.Client.Do, which is bypassed here; and
// the "permanent failed path" use cases (file://, gopher://, ftp://) that I added in that round can
// all be parsed successfully by url.Parse, and are on the scheme branch, so full green does not
// prove this path to be safe — it is a false guarantee.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // Illegal Percentage Perversion
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // Ports non-digital
		"http://[::1?access_token=" + leakProbeToken,                  // Brackets not matched
	}
	for _, raw := range cases {
		// First, make sure that this input** does** cause url.Parse to fail. If you don't do this,
		// the use case may move to another branch without notice (this is how the previous round of
		// false promises came about).
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q Should have parsed failed, otherwise this example did not cover the target branch", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q Should be verified failed", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// Confirm that the channel layer package does not take the address out either.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("Invalid address should be verified failed")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault Overrides the dial guard for the SMTP channel.
//
// The mail channel used the bare net.Dialer, which was the only gap in the SSRF defense: host
// filled with 169.254.169.254 or 127.0.0.1 would connect directly, while smtp.NewClient would fail
// to return the line that returned to the end by packing it into error and showing it through
// last_error in the delivery history interface—the semi-blind read language that other channels had
// already closed; the time difference between "connection denied vs. timeout" could also be used to
// detect ports.
//
// The TestMain global of this package enables AllowLocalTargetsEnv (a fake receiver on 127.0.0.1
// used by many use cases), so the use case must clear it itself—otherwise the guard will pass
// whether or not it is present, which is why the vulnerability was not discovered by any tests.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // Shut down escape = default behavior
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("Default should not be allowed to send mail back to the ring address.")
	}
	// The connection should not be established at all: the guard is stopped in the Control hook,
	// and the EHLO will never be sent out.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("The SMTP session has been created -- the guards are not working.")
	}
	// The error message should be able to guide the user how to release (native postfix relays are
	// validly configured).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("Rejection of information should indicate how to open it: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn is the reverse use case of pairing: when explicitly
// enabled, it must be able to deliver.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("SMTP should be capable of delivering: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("Can't see EHLO -- the session is not really created")
	}
}
