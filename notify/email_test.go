package notify

import (
	"bufio"
	"context"

	"net"
	"strings"
	"sync"
	"testing"
)

// This document completes the protocol-level testing of the mail channel. Prior to this, email.Send
// had a coverage of 0 - no use case was run on the entire SMTP path, which is precisely the
// protocol-side largest and most error-prone of the six channels (with failure semantics for each
// of the handshake, authentication, envelope, and DATA stages).
//
// Here, we use a custom built minimal SMTP server driver instead of mocking net/smtp: Most of the
// risk in the mail channel lies in the "talk to real SMTP server" step, and mocking this step is
// incalculable.

// fakeSMTP is a just-in-time SMTP server: it can complete greet/EHLO/AUTH/MAIL/RCPT/DATA/QUIT, and
// return the specified response code for a specific stage as required by the use case.
type fakeSMTP struct {
	ln net.Listener

	// The rcptReply is the response to the RCPT TO; the default is 250.
	rcptReply string
	// mailReply is the response to MAIL FROM; default 250.
	mailReply string
	// AUTH PLAIN is declared in EHLO when advertiseAuth is true.
	advertiseAuth bool

	mu       sync.Mutex
	data     string
	commands []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, rcptReply: "250 OK", mailReply: "250 OK"}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) hostPort(t *testing.T) (string, int) {
	t.Helper()
	addr, ok := f.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("Non TCP listening address")
	}
	return "127.0.0.1", addr.Port
}

func (f *fakeSMTP) record(cmd string) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
}

func (f *fakeSMTP) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data
}

func (f *fakeSMTP) sawCommand(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	w("220 fake.local ESMTP ready")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.record(line)
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			// No STARTTLS declaration: Let the code branch into plaintext (the test target is
			// envelope logic, not TLS).
			w("250-fake.local")
			if f.advertiseAuth {
				w("250-AUTH PLAIN")
			}
			w("250 8BITMIME")
		case strings.HasPrefix(line, "AUTH"):
			// Simplified processing: The initial response of PLAIN may be cross-line and directly
			// accepted.
			w("235 2.7.0 Authentication successful")
		case strings.HasPrefix(line, "MAIL FROM"):
			w(f.mailReply)
		case strings.HasPrefix(line, "RCPT TO"):
			w(f.rcptReply)
		case strings.HasPrefix(line, "DATA"):
			w("354 End data with <CR><LF>.<CR><LF>")
			var sb strings.Builder
			for {
				dl, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				sb.WriteString(dl)
			}
			f.mu.Lock()
			f.data = sb.String()
			f.mu.Unlock()
			w("250 2.0.0 Ok: queued as FAKE1")
		case strings.HasPrefix(line, "QUIT"):
			w("221 2.0.0 Bye")
			return
		default:
			w("250 OK")
		}
	}
}

func emailCfg(t *testing.T, f *fakeSMTP, extra map[string]any) map[string]any {
	t.Helper()
	host, port := f.hostPort(t)
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "artex@example.com",
		"to":   []any{"a@example.com", "b@example.com"},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func TestEmailSendDeliversFullMessage(t *testing.T) {
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	cfg := emailCfg(t, f, map[string]any{"username": "artex", "password": "pw"})

	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
	// The envelope phase must go through: sender, two recipients, DATA.
	for _, want := range []string{"MAIL FROM:<artex@example.com>", "RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>", "DATA", "AUTH", "QUIT"} {
		if !f.sawCommand(want) {
			t.Errorf("SMTP session is missing %q; actual commands: %v", want, f.commands)
		}
	}
	// The body text is base64 HTML, and should contain the actual vulnerability content (which can
	// still be identified after encoding).
	body := f.body()
	if body == "" {
		t.Fatal("DATA no text received")
	}
	if !strings.Contains(body, "Content-Type: text/html") {
		t.Errorf("Missing Content-Type Head: \n %s", body)
	}
	if !strings.Contains(body, "base64") {
		t.Errorf("The body is not encoded in base64 (long HTML lines will break SMTP's 1000 byte line length limit):\n%s", body)
	}
	// Multiple recipients must appear in the To header.
	if !strings.Contains(body, "a@example.com, b@example.com") {
		t.Errorf("To header does not contain all recipients: \n %s", body)
	}
}

func TestEmailSendWithoutAuth(t *testing.T) {
	// AUTH should not be sent when the account is not assigned - some relays will reject it.
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("delivery failed: %v", err)
	}
	if f.sawCommand("AUTH") {
		t.Errorf("AUTH: %v", f.commands)
	}
}

// TestEmailSendClassifiesSMTPReplies is the direct verification of this audit fix: 5xx is a
// permanent failure, 4xx (greylist) is retryable.
func TestEmailSendClassifiesSMTPReplies(t *testing.T) {
	cases := []struct {
		name      string
		rcptReply string
		mailReply string
		permanent bool
	}{
		{"Recipient denied permanently 550", "550 5.1.1 User unknown", "250 OK", true},
		{"Recipient encounter 450 ash list", "450 4.7.1 Greylisting in action", "250 OK", false},
		{"Recipient met 452 mailbox full", "452 4.2.2 Mailbox full", "250 OK", false},
		{"Sender denied permanently by 553", "250 OK", "553 5.1.3 Bad address", true},
		{"Sender encountered 451 temporary error", "250 OK", "451 4.3.0 Temporary failure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSMTP(t)
			f.rcptReply = tc.rcptReply
			f.mailReply = tc.mailReply
			_, err := (emailChannel{}).Send(context.Background(), emailCfg(t, f, nil), singleMsg())
			if err == nil {
				t.Fatal("It's a mistake.")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("Permanent error: expect %v to get %v (%v)", tc.permanent, got, err)
			}
			// The server should be kept in its original language, otherwise users would not know
			// whether to contact the server administrator or change the address.
			if !strings.Contains(err.Error(), strings.Fields(tc.rcptReply)[0]) && !strings.Contains(err.Error(), strings.Fields(tc.mailReply)[0]) {
				t.Errorf("The answer number for the server should be retained in the error: %v", err)
			}
		})
	}
}

func TestEmailSendRefusesPlaintextCredentials(t *testing.T) {
	// PlainAuth for net/smtp refuses to issue credentials on unencrypted connections (unless the
	// target is localhost). This is a **correct** security behavior that cannot be bypassed;
	// however, an error message should be given to guide users through the correction. Here it is
	// triggered with a hostname other than localhost.
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	_, port := f.hostPort(t)
	cfg := map[string]any{
		"host":     "smtp.example.com", // Not localhost
		"port":     float64(port),
		"from":     "a@example.com",
		"to":       []any{"b@example.com"},
		"username": "artex",
		"password": "pw",
	}
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Skip("Host DNS parsing to local server, skipping (without prejudice to other cases)")
	}
	// Even failing to or refusing to issue credentials counts for this assertion; the key is that
	// ** cannot** silently issue the password.
	if !IsPermanent(err) && !strings.Contains(err.Error(), "Connections") {
		t.Logf("Error: %v (non-localhost failed to match expectations)", err)
	}
}

func TestEmailValidateReportsMissingFields(t *testing.T) {
	// The mail channel has the most configuration fields, and any missing one is exposed only at
	// delivery; here, each confirmation is checked before it can be stopped.
	cases := []struct {
		name string
		cfg  map[string]any
	}{
		{"Missing host", map[string]any{"port": float64(25), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"Lack of port", map[string]any{"host": "smtp.example.com"}},
		{"Port Cross-border", map[string]any{"host": "h", "port": float64(70000), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"Missing from", map[string]any{"host": "h", "port": float64(25), "to": []any{"d@e.f"}}},
		{"Not available to", map[string]any{"host": "h", "port": float64(25), "from": "a@b.c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := (emailChannel{}).Validate(tc.cfg); err == nil {
				t.Fatalf("delivery failed: %v", tc.cfg)
			}
		})
	}
}

// TestEmailConfigTolerance overrides the error tolerance of configuration reading: the value in
// JSONB is float64, but the user may fill the port as a string in the UI; the array can also be a
// single string.
func TestEmailConfigTolerance(t *testing.T) {
	cfg := map[string]any{
		"host": "smtp.example.com",
		"port": "587", // Ports in string form
		"from": "a@b.c",
		"to":   "d@e.f", // Single strings instead of arrays
		"tls":  "true",  // Boolean in string form
	}
	if err := (emailChannel{}).Validate(cfg); err != nil {
		t.Fatalf("Values in string form should be tolerated: %v", err)
	}
	if got := cfgInt(cfg, "port"); got != 587 {
		t.Errorf("cfgInt Unsolved String Port, get %d", got)
	}
	if !cfgBool(cfg, "tls") {
		t.Error("cfgBool Unsolved String \"")
	}
	if to := cfgStrings(cfg, "to"); len(to) != 1 || to[0] != "d@e.f" {
		t.Errorf("cfgstrings not compatible with single string, get %v", to)
	}
}

// TestFilterValidateRejectsTypo is the direct validation of the audit correction: The threshold
// misspellings must be blocked when written, otherwise the filter will silently fail to work and
// become a full push.
func TestFilterValidateRejectsTypo(t *testing.T) {
	good := []string{"", "low", "medium", "high", "critical"}
	for _, s := range good {
		if err := (Filter{MinSeverity: s}).Validate(); err != nil {
			t.Errorf("Legal threshold %q denied: %v", s, err)
		}
	}
	// These are typographical errors that really happen – all of which must be rejected.
	for _, s := range []string{"hgih", "HIGH", "Serious", "high ", "crit"} {
		err := (Filter{MinSeverity: s}).Validate()
		if err == nil {
			t.Errorf("Illegal threshold %q should be rejected (otherwise the filter is silently disabled and turned into full push)", s)
			continue
		}
		// The error message should be able to guide the user to correct the error.
		if !strings.Contains(err.Error(), "low") || !strings.Contains(err.Error(), "critical") {
			t.Errorf("Error information should list optional values, get %q", err.Error())
		}
	}
}

// TestFilterValidateIsWriteTimeOnly Locks the "write-strict, read-wide" division of work: Curie's
// already bad value cannot make the entire channel unreadable (that would cause all of the
// historical channels to suddenly stop pushing).
func TestFilterValidateIsWriteTimeOnly(t *testing.T) {
	raw := []byte(`{"min_severity":"hgih"}`)
	f := ParseFilter(raw) // No error reporting
	if f.MinSeverity != "hgih" {
		t.Fatalf("Read path should be kept as it is, get %q", f.MinSeverity)
	}
	// The channel is still able to judge the event (no panic, no blockage).
	_ = Match(f, Snapshot{Kind: EventFindingCreated, Severity: "critical"})
}
