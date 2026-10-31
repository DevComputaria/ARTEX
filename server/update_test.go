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
		t.Errorf("5 次查询只应回源 1 次，实际 %d 次", calls)
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
		t.Errorf("force 应绕过缓存，期望回源 2 次，实际 %d 次", calls)
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
		t.Errorf("TTL 过期后应重新回源，期望 2 次，实际 %d 次", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github 不可达")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("期望返回错误")
	}
	// Failure results must also be cached for a while, otherwise GitHub will be unable to reach the
	// page and timeout every time the page is loaded.
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("期望返回错误")
	}
	if calls != 1 {
		t.Errorf("错误应短时缓存，期望回源 1 次，实际 %d 次", calls)
	}

	// But the TTL of a failure must be significantly shorter than that of a success, and the
	// network must heal itself quickly after recovery.
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("错误 TTL(%v) 必须短于成功 TTL(%v)", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("期望返回错误")
	}
	if calls != 2 {
		t.Errorf("错误 TTL 过期后应重试，期望 2 次，实际 %d 次", calls)
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
		t.Fatal("调用方已取消时应把错误透传给它")
	}

	// Key invariant: The time it was cancelled leaves no trace—there is no "cancelled" error in the
	// cache, and the previous good result is still there.
	if c.err != nil {
		t.Fatalf("取消错误不应写进缓存，得到 %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("缓存应保留上一次的好结果，得到 %+v", c.rel)
	}

	// That cancellation did not replace any new data, so the next visitor should return again—and
	// get the results properly, not overwhelmed by the previous cancellation.
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("取消之后的正常请求不应报错: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("应拿到正常结果，得到 %+v", rel)
	}
}
