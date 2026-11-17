package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache is the layer that protects GitHub quotas: unauthorized APIs are only allowed 60
// times/hour/IP, and the "new version" notification in the top bar tells you that it will be
// checked once every time a full page is loaded. Once the cache expires, users open several extra
// tab pages to run out of quota, and then they don't want to update.

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("5 queries should be returned only 1 time, actual %d", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// The user point "check for updates" must get real-time results, otherwise the newly released
	// version will wait until the cache expires before being visible.
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("Force should bypass the cache, expect to return 2 times, actual %d", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// The drop time is forwarded to just before expiration, simulating TTL to point.
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("TTL should revert to source after expiry, expected 2 times, actual %d", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("♪ Gishub Untraceable")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("Expecting a return error")
	}
	// Failure results must also be cached for a while, otherwise GitHub will be unable to reach the
	// page and timeout every time the page is loaded.
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("Expecting a return error")
	}
	if calls != 1 {
		t.Errorf("Errors should be cached shortly, expected to return source 1 time, actual %d", calls)
	}

	// But the TTL of a failure must be significantly shorter than that of a success, and the
	// network must heal itself quickly after recovery.
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("Error TTL(%v) must be shorter than successful TTl( %v)", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("Expecting a return error")
	}
	if calls != 2 {
		t.Errorf("Error TTL should be retry, expected 2 times, actual %d", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// This does not mean that GitHub is in trouble, and you should never write "cancelled" to the
	// cache — otherwise every visitor will receive a strange error for the next 30 minutes.
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // Let the cache expire, force it back to source

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("When the caller is canceled, pass on the error to it.")
	}

	// Key invariant: The time it was cancelled leaves no trace—there is no "cancelled" error in the
	// cache, and the previous good result is still there.
	if c.err != nil {
		t.Fatalf("Cancel error should not be written into cache, get %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("Cache should retain the last good result, get %+v", c.rel)
	}

	// That cancellation did not replace any new data, so the next visitor should return again—and
	// get the results properly, not overwhelmed by the previous cancellation.
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("The normal request after cancellation should not be misreported: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("Should have a normal result, get %+v", rel)
	}
}
