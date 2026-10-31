package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets Determines whether to allow messages to be delivered to the loopback /
// link-local address.
//
// These addresses are not where IM bots or public mail servers would appear, and they can call
// something sensitive: the management port of another service on the same machine, and the metadata
// endpoint of the cloud environment (169.254.169.254, which can read instance credentials). The
// delivery address is admin-assigned, but a second person in an XSS/CSRF-borrowed management
// session, or sharing the same JWT, can read back the response content by reconfigured — doJSON
// writes the first 200 bytes of the 4xx/5xx response body into last_error, and the delivery history
// interface returns it, which is a semi-blind read source.
//
// However, "local SMTP relays" (postfix on 127.0.0.1:25) are a common configuration for custom mail
// and can get you stuck in one go. So leave an explicit escape rather than hardcoded: Set
// ARTEX_NOTIFY_ALLOW_LOCAL=1 to allow it.
//
// The export to AllowLocalTargetsEnv is intended for testing to explicitly open it — the use case
// of this package and the server package uses a large httptest fake receiver on 127.0.0.1, which is
// blocked by the gatekeeper if not opened.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP reports whether the target IP belongs to an address range blocked by default.
//
// Only deny loopback, link-local (including cloud metadata 169.254.169.254), unspecified and
// broadcast. **Not deny** RFC1918 Private network: Intranet self-built Mattermost/SMTP relays are
// common legitimate uses, blocking them together would make the functionality immediately
// unavailable in real-world environments.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped IPv6 (::ffff:127.0.0.1) must be reset to IPv4 again, otherwise the check is
	// bypassed.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial is a Control hook of the http.Transport dialer that checks the target address
// when **a connection is established**.
//
// Why set the check at dial-up rather than just when saving the configuration: This is the final
// point of effect. It covers both circumstances where the configuration check is bypassed — DNS
// rebinding (resolved to public IP at check, and to intranet when connected) and redirection
// (although we have rejected cross-host jumping, with-host jumping can still point the path
// elsewhere).
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("无法解析目标地址 %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("拒绝投递到本机/链路本地地址 %s（如确需投递到本机服务，设置 %s=1）", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport Adds only one dial guard based on the default Transport. Clone preserves all
// default tuning (connection pool, HTTP/2, timeout, proxy, etc.) and avoids changing other behavior
// to add a check.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient is the client that is shared by all channels.
//
// Intentionally** not** reuse the global export proxy of the project (server-side GlobalProxy): The
// proxy is used for security-testing target traffic and often points to unstable tunnels; notification
// availability should not depend on instability in the target network. IM notifications can connect directly.
// Set the timeout to 15 seconds — slower than that is actually a failure.
//
// Reject cross-host redirects: The delivery addresses for this feature are in the form of a "fixed
// endpoint" and will not normally be redirected to another host; while the credentials for these
// hosts (DingTalk's access_token, WeCom's key, and Telegram's bot token) **are in the URL**; following a
// following cross-host jump is equivalent to passing the credentials to the redirect target.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("重定向次数过多")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("拒绝跨主机重定向（%s → %s）", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// The respBodyLimit limits the size of the read response body.
const respBodyLimit = 8 << 10

// doJSON sends a request and returns the length-limited response body.
//
// Sends an empty body if payload is nil (for GET or scenarios where the platform does not require a
// body). The key values in headers are added as they are, for use in custom headers of general
// Webhooks.
//
// Error classification is the core functionality of this function: network-layer failures with
// 5xx/408/429 are classified as "retryable", and the remaining 4xx as "permanent failures"—retrying
// a 403 simply logs the same error three times.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// Serialization failures are local bugs (configuration field type incorrect) and will
			// not improve even after retrying.
			return nil, Permanent(fmt.Errorf("构造请求体失败: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// URL invalid - usually a user typed the address incorrectly and is considered a permanent
		// failure. Here too, it is not possible to pass on the error text of err:url.Parse
		// containing the full address.
		return nil, Permanent(fmt.Errorf("请求地址非法: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// Connection refusal, DNS failure, and timeout are usually transient failures; let the
		// backoff policy retry them.
		//
		// The error text must be redacted before being returned. http.Client.Do returns
		// *url.Error, its Error() is `Op "full URL": baseline error`, and the credentials for this
		// function are **in the URL** (DingTalk access_token, WeCom key, Feishu hook ID, Telegram
		// /bot<token>/). Without redaction, the credentials would flow with this error
		// line to four places: last_error of notification_deliveries (in plaintext), the response
		// of the delivery history interface (**a mask that bypasses channel configuration**), the
		// service-side log, and the 502 text that the test interface sends back to the front-end.
		return nil, fmt.Errorf("请求失败: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("读取响应失败: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429 (rate limiting) and 408 (timeout) are worth retrying; the remaining 4xx are
	// configuration or permission issues, and there is no point in trying again.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("对方限流或超时 (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("对方服务异常 (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("对方拒绝请求 (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet The response body is compressed into a short line of text for error messages. The
// response may contain newlines and large amounts of space, and inserting it directly into
// last_error will cause the delivery history page to crash.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget Compress the delivery address to "scheme://host/…" for error messages.
//
// This is the only URL redaction in this package, and it is intentionally done **roughly
// enough**: except for scheme and host, everything else is discarded. The reason is that there is
// no "universal and safe" way to determine which part of a URL is a credential:
//
//	DingTalk credentials in query /robot/send?access_token=xxx; WeCom credentials in query
//	/cgi-bin/webhook/send?key=xxx Feishu credentials at the end of ** path**
//	/open-apis/bot/v2/hook/<hook_id> Telegram credentials in the middle of ** path**
//	/bot<token>/sendMessage
//
// If you want to "keep only the useful part" you have to patch by channel, and missing any one is a
// credential leak. Keeping the host is enough for the check (DNS cannot be resolved, not connected,
// certificates are not correct can be located), which bot is identified by the mask tail prompt in
// the channel configuration.
//
// Returns a fixed placeholder on parsing failures—never returning the original string.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(地址不可解析)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError Strips the address from the transport layer error, leaving only the
// underlying cause.
//
// *url.Error is structured as {Op, URL, Err}, and Error() will print the URL together. Here, the
// Err field is explicitly taken, bypassing Error() — more reliable than subsequent string
// replacement, because the substitution is to correctly handle the various variants of the URL
// encoding/translating, which are easily missed.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: 未知错误", uerr.Op, host)
	}
	// Non-*url.Errors (such as errors returned by redirect policies) may also be addressed,
	// uniformly out of sensitivity.
	return redactURLsInText(err.Error())
}

// redactURLsInText replaces HTTP(S) addresses in text with redacted forms.
//
// Used to pinpoint errors that cannot be found in the structured fields (redirect policy errors,
// customization errors of third-party libraries). Only recognizes http/https prefixes, separated by
// spaces and quotation marks — the address does not contain these characters.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
