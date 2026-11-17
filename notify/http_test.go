package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// This document covers HTTP layer error classification for doJSON.
//
// Why separately: Each channel adapter handles its own business error codes (DingTalk errcode,
// Feishu code, Telegram ok field), while the **HTTP layer** is classified by doJSON,  and both are
// independent lines of defense. Without this, a transit gateway returning 503 would be treated as a
// permanent failure and abandoned immediately; while a 403 would be treated as retryable and
// avoided three rounds without any consequences.

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200. Success is not a mistake.", 200, false},
		{"429 ratting retry", 429, false},
		{"408. Requesttimeoutretry", 408, false},
		{"500 Server Error Retry", 500, false},
		{"502 Gateway Error Retry", 502, false},
		{"503 service not availableretry", 503, false},
		{"400 Parameter Error Permanently Failed", 400, true},
		{"401 Permanent failure of opinio juris", 401, true},
		{"403 Permanent Failure to Ban Access", 403, true},
		{"404 Address does not have a permanent failure", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2x should not be misreported: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Non-two x reportable error")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("Permanent error in determination of HTTP %d: expecting %v to get %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// The status code must be present in the error, otherwise the user cannot tell whether
			// he has made a mistake or is on the wrong side.
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("Error message should be loaded with HTTP status code %d, get %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet Overrides the snippet: the error statement returned to the end
// must be brought back, otherwise the user only knows "failed" and does not know why the end
// refused.
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("It's a mistake.")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("Error message should be brought back to the description of the peer, get %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded Bounded snippet form: It responds to the end as it is in
// the last_error column and front-end table, multiline/oversize breaks layout and load.
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// Responsiveness with line breaks, tabs and 5000-character superlong content.
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("It's a mistake.")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("Error message should be pressed to a single line, get %q", msg)
	}
	// snippets are limited to 200 characters + fixed prefix, and the total must be much smaller
	// than the original response.
	if len(msg) > 400 {
		t.Errorf("Error message is too long (%d bytes) to be intercepted by snippet: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse Confirms that there is a read limit: if the end-user exception
// returns oversized content, the entire response cannot be read into memory (a last_error is stored
// for each entry in the delivery history).
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("It's a mistake.")
	}
	if len(err.Error()) > 400 {
		t.Errorf("Exceeding response should be limited to long read and cut, error message length %d", len(err.Error()))
	}
}
