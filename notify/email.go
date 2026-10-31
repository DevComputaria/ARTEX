package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout / emailSessionTimeout restricts the connection and the entire SMTP session
// respectively. net/smtp itself has no timeout mechanism, and without both, a stuck end would leave
// the delivery goroutine hanging there forever — and the dispatcher is a single goroutine
// serialized, which would put the entire notification system on hold.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel implements SMTP mail delivery.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// Mail has no platform limits, but should not be used to scan; give a loose default.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// Mask only the password. SMTP host, account number, and recipient are not secrets, masking them
// only makes editing more difficult.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// Any of these changes requires a re-entry of the password—and the "disable TLS" step must be
// explicitly accompanied by credentials, not changed by hand.
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("缺少 SMTP 服务器地址")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("SMTP 端口无效（应为 1-65535）")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("缺少发件人地址")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("至少需要一个收件人地址")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// STARTTLS: Upgraded end-to-end support. Cannot issue credentials in plaintext sessions (see
	// auth below).
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS 失败: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth refuses to send credentials on unencrypted connections (unless the
			// target is localhost). This is a **correct** security behavior that cannot be
			// bypassed, but it needs to be translated clearly — otherwise users will see
			// "unencrypted connection" and not know what to do.
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("拒发凭据：连接未加密。请启用 TLS，或改用 465 端口(隐式 TLS)，或把「启用 TLS」勾上 (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP 认证失败: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("发件人 %s 被拒", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("收件人 %s 被拒", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA 失败: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("写入邮件正文失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("提交邮件失败: %w", err)
	}
	// Quit failure does not affect the fact that "mail has been received by the server", so its
	// error is ignored.
	_ = client.Quit()
	// The message is not cut in length (HTML body is sent entirely) and the whole batch is
	// considered delivered.
	return len(m.Items), nil
}

// emailDial establishes an SMTP connection.
//
// implicitTLS=true Goes to 465 This is the "connected-to-TLS" way; false Goes to 25/587 Openly
// connect and then STARTTLS.
//
// The session expiration is set at the **connection creation** point (rather than afterwards),
// because the net/smtp client hides the base layer connection in an unexported field, which is not
// accessible externally; once the connection is submitted, it can only be reclaimed by a predefined
// deadline. This also covers the blocking of the handshake phase. Control holds blockInternalDial
// together with the HTTP channel. If not held, SMTP is the entire SSRF protection loophole: host
// fills 169.254.169.254 or 127.0.0.1 to connect directly, and if the smtp.NewClient handshake
// fails, the line returned to the end is packed in error, and last_error is returned by the
// delivery history interface, constituting semi-blind read; the time difference between "connection
// denied vs. timeout" is also used to detect ports. The dial-up phase is the final effective point,
// and also covers DNS rebinding.
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("连接 SMTP 服务器失败: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP 握手失败: %w", err)
	}
	return client, nil
}

// smtpStageError Divides a stage failure into a retryable and a permanent failure by SMTP response
// code.
//
// Why it is necessary to distinguish: The semantics of SMTP 4xx and 5xx are completely different—
// - 4xx (450 graylist, 451 local error, 452 insufficient storage) is a **temporary** refusal, the
// normal practice is to try again later; graylists, in particular, are encountered almost every
// first submission.
// - 5xx (550 user does not exist, 553 address is illegal) is a permanent refusal, retrying is
// pointless.
//
// If all of the pushes fail permanently, a mail server with graylist enabled will fail every push
// on the first attempt - which is exactly where automatic retry should work best. The answer code
// takes the first three digits of the incorrect text; if it cannot get the code, it is treated as
// retryable (better try more than once, rather than condemning a possible momentary failure to
// failure).
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode Gets the three-bit response code from the SMTP error text, not the prefix, and
// returns 0. net/smtp does not export the error code field, but only gets it from the text; format
// is "450 4.7.1...".
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage Assembles a complete RFC 5322 message.
//
// The body is encoded in base64 for two reasons: first, because SMTP requires that a single line be
// no longer than 1000 bytes, and HTML body (especially summary messages) are prone to overly long
// lines; and second, because base64 naturally does not show lines beginning with ".", avoiding the
// hassle of SMTP dot-translation.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// Non-ASCII subjects must use RFC 2047 encoding, otherwise mail clients may display mojibake.
	// by the client.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// There is no hard limit on the length of the message, so it is not truncated.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// base64 wraps by 76 characters, compliant with RFC 2045.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
