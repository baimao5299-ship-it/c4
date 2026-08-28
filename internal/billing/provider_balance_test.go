// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeBalanceAdapter struct {
	configured atomic.Bool
	mu         sync.Mutex
	result     ProviderBalance
	err        error
	calls      int
	started    chan struct{}
	release    chan struct{}
}

func (f *fakeBalanceAdapter) Provider() string { return "fake" }

func (f *fakeBalanceAdapter) EndpointConfigured() bool { return f.configured.Load() }

func (f *fakeBalanceAdapter) Fetch(ctx context.Context, _ BalanceAccount) (ProviderBalance, error) {
	f.mu.Lock()
	f.calls++
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
	}
	release := f.release
	result, err := f.result, f.err
	f.mu.Unlock()
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ProviderBalance{}, ctx.Err()
		}
	}
	return result, err
}

func (f *fakeBalanceAdapter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func testClock() (*time.Time, func() time.Time) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	return &now, func() time.Time { return now }
}

func TestBalanceCacheUnconfiguredIsExplicit(t *testing.T) {
	_, now := testClock()
	c, err := NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, Now: now})
	require.NoError(t, err)
	s := c.Get(context.Background(), BalanceAccount{ID: 7, Provider: "relay"}, nil)
	require.Equal(t, BalanceStatusUnconfigured, s.Status)
	require.Equal(t, BalanceErrorNoEndpoint, s.ErrorCode)
	require.Empty(t, s.Amount, "未配置余额接口不能伪造为 0")
}

func TestBalanceCacheTreatsTypedNilAdapterAsUnconfigured(t *testing.T) {
	_, now := testClock()
	c, err := NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, Now: now})
	require.NoError(t, err)
	var adapter *fakeBalanceAdapter
	snapshot := c.Get(context.Background(), BalanceAccount{ID: 70, Provider: "relay"}, adapter)
	require.Equal(t, BalanceStatusUnconfigured, snapshot.Status)
	require.Equal(t, BalanceErrorNoEndpoint, snapshot.ErrorCode)
}

func TestNilBalanceCacheDoesNotPanic(t *testing.T) {
	var cache *BalanceCache
	snapshot := cache.Get(context.Background(), BalanceAccount{ID: 71, Provider: "relay"}, nil)
	require.Equal(t, BalanceStatusUnavailable, snapshot.Status)
	require.Equal(t, BalanceErrorNoEndpoint, snapshot.ErrorCode)
	cache.Invalidate("relay", 71)
}

func TestBalanceCacheFreshLowAndExpiry(t *testing.T) {
	clock, now := testClock()
	c, err := NewBalanceCache(BalanceCacheConfig{
		TTL: time.Minute, StaleIfError: 2 * time.Minute, LowBalanceThreshold: "10.00", Now: now,
	})
	require.NoError(t, err)
	a := &fakeBalanceAdapter{result: ProviderBalance{Amount: "9.999", Currency: "USD"}}
	a.configured.Store(true)
	s := c.Get(context.Background(), BalanceAccount{ID: 1, Provider: "fake"}, a)
	require.Equal(t, BalanceStatusFresh, s.Status)
	require.Equal(t, "9.999", s.Amount)
	require.Equal(t, "USD", s.Currency)
	require.True(t, s.Low, "低余额使用精确比较，不受浮点舍入影响")
	require.Equal(t, *clock, s.CheckedAt)
	require.Equal(t, clock.Add(time.Minute), s.ExpiresAt)

	// Fresh cache hit does not call the provider again.
	_ = c.Get(context.Background(), BalanceAccount{ID: 1, Provider: "fake"}, a)
	require.Equal(t, 1, a.callCount())
}

func TestBalanceCacheStaleIfErrorThenUnavailable(t *testing.T) {
	clock, now := testClock()
	c, err := NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, StaleIfError: 2 * time.Minute, Now: now})
	require.NoError(t, err)
	a := &fakeBalanceAdapter{result: ProviderBalance{Amount: "12.50"}}
	a.configured.Store(true)
	account := BalanceAccount{ID: 2, Provider: "fake"}
	first := c.Get(context.Background(), account, a)
	require.Equal(t, BalanceStatusFresh, first.Status)

	*clock = clock.Add(90 * time.Second)
	a.mu.Lock()
	a.err = errUpstreamForTest{}
	a.mu.Unlock()
	stale := c.Get(context.Background(), account, a)
	require.Equal(t, BalanceStatusStale, stale.Status)
	require.Equal(t, "12.50", stale.Amount, "刷新失败期间保留最后成功值")
	require.Equal(t, BalanceErrorUpstream, stale.ErrorCode)
	require.Equal(t, *clock, stale.AttemptedAt)
	require.Equal(t, first.CheckedAt, stale.CheckedAt)

	*clock = clock.Add(2 * time.Minute)
	unavailable := c.Get(context.Background(), account, a)
	require.Equal(t, BalanceStatusUnavailable, unavailable.Status)
	require.Empty(t, unavailable.Amount, "过期窗口结束后不继续使用旧余额")
	require.Equal(t, BalanceErrorUpstream, unavailable.ErrorCode)
}

func TestBalanceCacheInvalidRefreshKeepsStaleValue(t *testing.T) {
	clock, now := testClock()
	c, err := NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, StaleIfError: time.Minute, Now: now})
	require.NoError(t, err)
	a := &fakeBalanceAdapter{result: ProviderBalance{Amount: "3.00"}}
	a.configured.Store(true)
	account := BalanceAccount{ID: 4, Provider: "fake"}
	first := c.Get(context.Background(), account, a)
	require.Equal(t, BalanceStatusFresh, first.Status)

	*clock = clock.Add(90 * time.Second)
	a.mu.Lock()
	a.result = ProviderBalance{Amount: "not-a-number"}
	a.mu.Unlock()
	stale := c.Get(context.Background(), account, a)
	require.Equal(t, BalanceStatusStale, stale.Status)
	require.Equal(t, "3.00", stale.Amount)
	require.Equal(t, BalanceErrorInvalidValue, stale.ErrorCode)
}

func TestBalanceCacheCancellationPreservesLastSuccess(t *testing.T) {
	clock, now := testClock()
	c, err := NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, StaleIfError: 2 * time.Minute, Now: now})
	require.NoError(t, err)
	a := &fakeBalanceAdapter{result: ProviderBalance{Amount: "8"}}
	a.configured.Store(true)
	account := BalanceAccount{ID: 8, Provider: "fake"}
	first := c.Get(context.Background(), account, a)
	require.Equal(t, BalanceStatusFresh, first.Status)

	*clock = clock.Add(90 * time.Second)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	a.mu.Lock()
	a.started, a.release = started, release
	a.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan BalanceSnapshot, 1)
	go func() { done <- c.Get(ctx, account, a) }()
	<-started
	cancel()
	got := <-done
	require.Equal(t, BalanceStatusUnavailable, got.Status)
	require.Equal(t, BalanceErrorCanceled, got.ErrorCode)
	close(release)

	// The cancelled call did not erase the old value. A subsequent refresh may still
	// serve that value as stale while it rechecks the provider.
	a.mu.Lock()
	a.started, a.release = nil, nil
	a.err = errUpstreamForTest{}
	a.mu.Unlock()
	stale := c.Get(context.Background(), account, a)
	require.Equal(t, BalanceStatusStale, stale.Status)
	require.Equal(t, "8", stale.Amount)
}

type errUpstreamForTest struct{}

func (errUpstreamForTest) Error() string { return "upstream failed" }

func TestBalanceCacheCoalescesConcurrentRefresh(t *testing.T) {
	_, now := testClock()
	c, err := NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, Now: now})
	require.NoError(t, err)
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	a := &fakeBalanceAdapter{result: ProviderBalance{Amount: "42"}, started: started, release: release}
	a.configured.Store(true)
	account := BalanceAccount{ID: 9, Provider: "fake"}

	results := make(chan BalanceSnapshot, 2)
	go func() { results <- c.Get(context.Background(), account, a) }()
	<-started
	go func() { results <- c.Get(context.Background(), account, a) }()
	select {
	case <-started:
		t.Fatal("第二个并发请求绕过了 refresh 合并")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for range 2 {
		s := <-results
		require.Equal(t, BalanceStatusFresh, s.Status)
	}
	require.Equal(t, 1, a.callCount())
}

func TestBalanceCacheFailureCooldownAvoidsRefreshStorm(t *testing.T) {
	clock, now := testClock()
	c, err := NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, FailureCooldown: 30 * time.Second, Now: now})
	require.NoError(t, err)
	a := &fakeBalanceAdapter{err: errUpstreamForTest{}}
	a.configured.Store(true)
	account := BalanceAccount{ID: 10, Provider: "fake"}
	first := c.Get(context.Background(), account, a)
	require.Equal(t, BalanceStatusUnavailable, first.Status)
	require.Equal(t, 1, a.callCount())
	second := c.Get(context.Background(), account, a)
	require.Equal(t, BalanceStatusUnavailable, second.Status)
	require.Equal(t, 1, a.callCount(), "失败冷却窗口内不重复打上游")
	*clock = clock.Add(31 * time.Second)
	_ = c.Get(context.Background(), account, a)
	require.Equal(t, 2, a.callCount(), "冷却结束后允许再次检查")
}

func TestBalanceCacheInvalidateOnCredentialChange(t *testing.T) {
	clock, now := testClock()
	c, err := NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, Now: now})
	require.NoError(t, err)
	a := &fakeBalanceAdapter{result: ProviderBalance{Amount: "5"}}
	a.configured.Store(true)
	account := BalanceAccount{ID: 11, Provider: "fake", BaseURL: "https://relay.example", UpstreamKey: "old"}
	first := c.Get(context.Background(), account, a)
	require.Equal(t, "5", first.Amount)
	a.mu.Lock()
	a.result = ProviderBalance{Amount: "9"}
	a.mu.Unlock()
	c.Invalidate(account.Provider, account.ID)
	*clock = clock.Add(time.Second)
	got := c.Get(context.Background(), account, a)
	require.Equal(t, "9", got.Amount)
	require.Equal(t, 2, a.callCount())
}

func TestBalanceCacheKeyTracksCredentialAndEndpoint(t *testing.T) {
	_, now := testClock()
	c, err := NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, Now: now})
	require.NoError(t, err)
	a := &fakeBalanceAdapter{result: ProviderBalance{Amount: "1"}}
	a.configured.Store(true)
	first := c.Get(context.Background(), BalanceAccount{ID: 12, Provider: "fake", UpstreamKey: "one"}, a)
	require.Equal(t, "1", first.Amount)
	a.mu.Lock()
	a.result = ProviderBalance{Amount: "2"}
	a.mu.Unlock()
	second := c.Get(context.Background(), BalanceAccount{ID: 12, Provider: "fake", UpstreamKey: "two"}, a)
	require.Equal(t, "2", second.Amount, "凭据指纹变化不得命中旧快照")
	require.Equal(t, 2, a.callCount())
}

func TestBalanceCacheMaxEntriesIsBounded(t *testing.T) {
	_, now := testClock()
	c, err := NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, MaxEntries: 2, Now: now})
	require.NoError(t, err)
	a := &fakeBalanceAdapter{result: ProviderBalance{Amount: "1"}}
	a.configured.Store(true)
	for id := int64(1); id <= 3; id++ {
		_ = c.Get(context.Background(), BalanceAccount{ID: id, Provider: "fake"}, a)
	}
	c.mu.Lock()
	require.LessOrEqual(t, len(c.entries), 2)
	c.mu.Unlock()
}

func TestBalanceCacheHardBoundsConcurrentMisses(t *testing.T) {
	_, now := testClock()
	c, err := NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, MaxEntries: 1, Now: now})
	require.NoError(t, err)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	a := &fakeBalanceAdapter{result: ProviderBalance{Amount: "1"}, started: started, release: release}
	a.configured.Store(true)

	first := make(chan BalanceSnapshot, 1)
	go func() { first <- c.Get(context.Background(), BalanceAccount{ID: 1, Provider: "fake"}, a) }()
	<-started
	second := make(chan BalanceSnapshot, 1)
	go func() { second <- c.Get(context.Background(), BalanceAccount{ID: 2, Provider: "fake"}, a) }()
	<-started
	c.mu.Lock()
	entries := len(c.entries)
	c.mu.Unlock()
	require.LessOrEqual(t, entries, 1)

	close(release)
	require.Equal(t, BalanceStatusFresh, (<-first).Status)
	require.Equal(t, BalanceStatusFresh, (<-second).Status)
	c.mu.Lock()
	require.LessOrEqual(t, len(c.entries), 1)
	c.mu.Unlock()
}

func TestHTTPJSONAdapterMapsAmountAndAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"balance": "123.456", "currency": "USD"}})
	}))
	defer srv.Close()
	a := HTTPJSONAdapter{
		NameValue: "relay", Endpoint: srv.URL, Auth: HTTPAuthBearer,
		BalancePath: "data.balance", CurrencyPath: "data.currency", Client: srv.Client(),
	}
	aResult, err := a.Fetch(context.Background(), BalanceAccount{ID: 1, UpstreamKey: "secret"})
	require.NoError(t, err)
	require.Equal(t, "123.456", aResult.Amount)
	require.Equal(t, "USD", aResult.Currency)
	require.Equal(t, "Bearer secret", gotAuth)
}

func TestHTTPJSONAdapterDefaultClientDoesNotFollowRedirectWithKey(t *testing.T) {
	var destinationAuth string
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, nil, destination.URL, http.StatusFound)
	}))
	defer source.Close()

	a := HTTPJSONAdapter{NameValue: "relay", Endpoint: source.URL, Auth: HTTPAuthBearer}
	_, err := a.Fetch(context.Background(), BalanceAccount{ID: 1, UpstreamKey: "secret"})
	require.Error(t, err)
	require.Empty(t, destinationAuth, "redirect destination must never receive the upstream key")
}

func TestHTTPJSONAdapterClassifiesHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   BalanceErrorCode
	}{
		{http.StatusUnauthorized, BalanceErrorAuth},
		{http.StatusTooManyRequests, BalanceErrorRateLimited},
		{http.StatusBadGateway, BalanceErrorUpstream},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }))
			defer srv.Close()
			a := HTTPJSONAdapter{NameValue: "relay", Endpoint: srv.URL, Client: srv.Client()}
			_, err := a.Fetch(context.Background(), BalanceAccount{})
			require.Error(t, err)
			require.Equal(t, tc.want, classifyBalanceError(err))
		})
	}
}

func TestHTTPJSONAdapterRequiresKeyForAuthenticatedMode(t *testing.T) {
	a := HTTPJSONAdapter{NameValue: "relay", Endpoint: "https://example.invalid/balance", Auth: HTTPAuthBearer}
	_, err := a.Fetch(context.Background(), BalanceAccount{})
	require.Error(t, err)
	require.Equal(t, BalanceErrorAuth, classifyBalanceError(err))
}

func TestHTTPJSONAdapterRejectsNonHTTPEndpoint(t *testing.T) {
	a := HTTPJSONAdapter{NameValue: "relay", Endpoint: "file:///tmp/balance.json"}
	_, err := a.Fetch(context.Background(), BalanceAccount{})
	require.Error(t, err)
	require.NotEqual(t, BalanceErrorAuth, classifyBalanceError(err))
}

func TestHTTPJSONAdapterRejectsUnknownConfiguration(t *testing.T) {
	a := HTTPJSONAdapter{NameValue: "relay", Endpoint: "https://example.invalid", Auth: HTTPAuthStyle("basic")}
	require.Error(t, a.Validate())
	a = HTTPJSONAdapter{NameValue: "relay", Endpoint: "https://example.invalid", Method: http.MethodPatch}
	require.Error(t, a.Validate())
	a = HTTPJSONAdapter{Endpoint: "https://example.invalid"}
	require.Error(t, a.Validate())
}

func TestFetchAutoBalanceUsesCommonRelayShape(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/api/user/self" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		require.Equal(t, "Bearer relay-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"balance":"12.34","currency":"USD"}}`))
	}))
	defer server.Close()

	result, err := FetchAutoBalance(context.Background(), BalanceAccount{
		ID: 1, BaseURL: server.URL, UpstreamKey: "relay-key",
	}, server.Client())
	require.NoError(t, err)
	require.Equal(t, "12.34", result.Amount)
	require.Equal(t, "USD", result.Currency)
	require.Equal(t, []string{"/v1/usage", "/api/user/self"}, paths)
}

func TestFetchAutoBalanceUsesCCSwitchUsageShape(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/v1/usage" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		require.Equal(t, "Bearer relay-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"remaining":"8.80","unit":"USD"}`))
	}))
	defer server.Close()

	result, err := FetchAutoBalance(context.Background(), BalanceAccount{
		ID: 1, BaseURL: server.URL, UpstreamKey: "relay-key",
	}, server.Client())
	require.NoError(t, err)
	require.Equal(t, "8.80", result.Amount)
	require.Equal(t, "USD", result.Currency)
	require.Equal(t, []string{"/v1/usage"}, paths)
}

func TestFetchAutoBalanceReportsAuthenticationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := FetchAutoBalance(context.Background(), BalanceAccount{
		ID: 1, BaseURL: server.URL, UpstreamKey: "relay-key",
	}, server.Client())
	require.Error(t, err)
	require.Equal(t, BalanceErrorAuth, classifyBalanceError(err))
}

func TestHTTPJSONAdapterRejectsHead(t *testing.T) {
	a := HTTPJSONAdapter{NameValue: "relay", Endpoint: "https://example.invalid", Method: http.MethodHead}
	require.ErrorContains(t, a.Validate(), "HEAD")
}

func TestHTTPJSONAdapterRejectsTrailingJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"balance":"1"}{"balance":"2"}`))
	}))
	defer srv.Close()
	a := HTTPJSONAdapter{NameValue: "relay", Endpoint: srv.URL, BalancePath: "balance", Client: srv.Client()}
	_, err := a.Fetch(context.Background(), BalanceAccount{})
	require.Error(t, err)
	require.Equal(t, BalanceErrorUpstream, classifyBalanceError(err))
}

func TestHTTPJSONAdapterRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"balance":"123456789"}`))
	}))
	defer srv.Close()
	a := HTTPJSONAdapter{NameValue: "relay", Endpoint: srv.URL, BalancePath: "balance", MaxResponseSize: 8, Client: srv.Client()}
	_, err := a.Fetch(context.Background(), BalanceAccount{})
	require.Error(t, err)
	require.Equal(t, BalanceErrorUpstream, classifyBalanceError(err))
}

func TestHTTPJSONAdapterHonorsAdapterTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	a := HTTPJSONAdapter{
		NameValue: "relay", Endpoint: srv.URL, BalancePath: "balance",
		Timeout: 10 * time.Millisecond, Client: srv.Client(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := a.Fetch(ctx, BalanceAccount{})
	require.Error(t, err)
	require.Equal(t, BalanceErrorTimeout, classifyBalanceError(err))
}

func TestBalanceErrorCodeDistinguishesDeadline(t *testing.T) {
	require.Equal(t, BalanceErrorTimeout, errorCode(context.DeadlineExceeded))
	require.Equal(t, BalanceErrorCanceled, errorCode(context.Canceled))
	require.Equal(t, BalanceErrorTimeout, classifyBalanceError(context.DeadlineExceeded))
	require.Equal(t, BalanceErrorCanceled, classifyBalanceError(context.Canceled))
}

func TestBalanceAdapterRegistryRejectsAmbiguousEntries(t *testing.T) {
	a := &fakeBalanceAdapter{}
	a.configured.Store(true)
	_, err := NewBalanceAdapterRegistry(a, a)
	require.Error(t, err)
	var typedNil *fakeBalanceAdapter
	_, err = NewBalanceAdapterRegistry(typedNil)
	require.Error(t, err)
}

func TestBalanceAdapterRegistryListsProvidersDeterministically(t *testing.T) {
	a := &fakeBalanceAdapter{}
	a.configured.Store(true)
	first := namedBalanceAdapter{name: "z", delegate: a}
	second := namedBalanceAdapter{name: "a", delegate: a}
	r, err := NewBalanceAdapterRegistry(first, second)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "z"}, r.Providers())
	got, ok := r.Get(" z ")
	require.True(t, ok)
	require.Equal(t, "z", got.Provider())
}

type namedBalanceAdapter struct {
	name     string
	delegate BalanceAdapter
}

func (a namedBalanceAdapter) Provider() string         { return a.name }
func (a namedBalanceAdapter) EndpointConfigured() bool { return a.delegate.EndpointConfigured() }
func (a namedBalanceAdapter) Fetch(ctx context.Context, account BalanceAccount) (ProviderBalance, error) {
	return a.delegate.Fetch(ctx, account)
}

func TestBalanceCacheRejectsUnsafeConfigurationAndValues(t *testing.T) {
	_, now := testClock()
	_, err := NewBalanceCache(BalanceCacheConfig{TTL: 0, Now: now})
	require.Error(t, err)
	_, err = NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, LowBalanceThreshold: "NaN", Now: now})
	require.Error(t, err)

	c, err := NewBalanceCache(BalanceCacheConfig{TTL: time.Minute, Now: now})
	require.NoError(t, err)
	a := &fakeBalanceAdapter{result: ProviderBalance{Amount: "-1"}}
	a.configured.Store(true)
	s := c.Get(context.Background(), BalanceAccount{ID: 3, Provider: "fake"}, a)
	require.Equal(t, BalanceStatusUnavailable, s.Status)
	require.Equal(t, BalanceErrorInvalidValue, s.ErrorCode)
}
