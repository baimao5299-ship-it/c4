// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/pkg/httpx"
	"github.com/stretchr/testify/require"
)

type runtimeTestRoundTripper struct{ label string }

func (r *runtimeTestRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(r.label))}, nil
}

type typedNilRuntimeRoundTripper struct{}

func (*typedNilRuntimeRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, nil
}

func newRuntimeTest(t *testing.T) (*proxyTransportRuntime, *runtimeTestRoundTripper, *runtimeTestRoundTripper) {
	t.Helper()
	initialGateway := &runtimeTestRoundTripper{label: "gateway-old"}
	initialCodex := &runtimeTestRoundTripper{label: "codex-old"}
	gateway, err := httpx.NewSwitchableTransport(initialGateway)
	require.NoError(t, err)
	codex, err := httpx.NewSwitchableTransport(initialCodex)
	require.NoError(t, err)
	buildGateway := func(f httpx.ProxyFuncs) http.RoundTripper {
		return &runtimeTestRoundTripper{label: f.Scheme + "-gateway"}
	}
	buildCodex := func(f httpx.ProxyFuncs, tls bool) http.RoundTripper {
		return &runtimeTestRoundTripper{label: f.Scheme + "-codex-tls-" + map[bool]string{true: "on", false: "off"}[tls]}
	}
	r := newProxyTransportRuntime(config.UpstreamConfig{DialTimeout: 1}, "http://old:1", httpx.ProxyFuncs{Scheme: "http"}, gateway, codex, buildGateway, buildCodex)
	return r, initialGateway, initialCodex
}

func TestProxyRuntimeFailedProbeKeepsBothRoutes(t *testing.T) {
	r, oldGateway, oldCodex := newRuntimeTest(t)
	r.probe = func(context.Context, http.RoundTripper) error { return context.DeadlineExceeded }
	err := r.Apply(context.Background(), "http://new:2")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Same(t, oldGateway, r.gateway.Current())
	require.Same(t, oldCodex, r.codex.Current())
	require.Equal(t, "http://old:1", r.ProxyURL())
	_, _, lastErr, _ := r.ProxySummary()
	require.Contains(t, lastErr, "context deadline exceeded")
}

func TestProxyRuntimeRecordsInvalidProxyInput(t *testing.T) {
	r, _, _ := newRuntimeTest(t)
	err := r.Apply(context.Background(), "http://missing-port")
	require.Error(t, err)
	_, _, lastErr, lastAt := r.ProxySummary()
	require.Equal(t, err.Error(), lastErr)
	require.False(t, lastAt.IsZero())
}

func TestProxyRuntimeSlowProbeDoesNotBlockStats(t *testing.T) {
	r, _, _ := newRuntimeTest(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	r.probe = func(ctx context.Context, _ http.RoundTripper) error {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	applyDone := make(chan error, 1)
	go func() { applyDone <- r.Apply(context.Background(), "http://new:2") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("proxy probe did not start")
	}
	statsDone := make(chan struct{})
	go func() {
		_ = r.Stats()
		close(statsDone)
	}()
	select {
	case <-statsDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("runtime stats blocked behind proxy probe")
	}
	close(release)
	select {
	case err := <-applyDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("proxy switch did not finish")
	}
}

func TestProxyRuntimeProbePanicKeepsExistingRoute(t *testing.T) {
	r, oldGateway, oldCodex := newRuntimeTest(t)
	r.probe = func(context.Context, http.RoundTripper) error { panic("fixture probe panic") }
	err := r.Apply(context.Background(), "http://new:2")
	require.Error(t, err)
	require.Contains(t, err.Error(), "proxy health probe panicked")
	require.Same(t, oldGateway, r.gateway.Current())
	require.Same(t, oldCodex, r.codex.Current())
	require.Equal(t, "http://old:1", r.ProxyURL())
}

func TestProxyRuntimeRejectsTypedNilBuilderResult(t *testing.T) {
	r, _, _ := newRuntimeTest(t)
	r.probe = nil
	var typedNil *typedNilRuntimeRoundTripper
	r.buildGateway = func(httpx.ProxyFuncs) http.RoundTripper { return typedNil }

	err := r.Apply(context.Background(), "http://new:2")
	require.Error(t, err)
	require.Contains(t, err.Error(), "gateway transport builder returned nil")
	require.Equal(t, "http://old:1", r.ProxyURL())
	require.Equal(t, "not_checked", r.Stats().(map[string]any)["probe_state"])
}

func TestProxyRuntimeSwapsAfterProbeAndMasksCredentials(t *testing.T) {
	r, _, _ := newRuntimeTest(t)
	r.probe = func(context.Context, http.RoundTripper) error { return nil }
	require.NoError(t, r.Apply(context.Background(), "http://new:2"))
	require.Equal(t, "http://new:2", r.ProxyURL())
	summary, configured, lastErr, _ := r.ProxySummary()
	require.True(t, configured)
	require.Equal(t, "http://new:2", summary)
	require.Empty(t, lastErr)
	require.NotContains(t, summary, "@")
}

func TestProxyRuntimeSameAddressReprobesCurrentRoutes(t *testing.T) {
	r, oldGateway, oldCodex := newRuntimeTest(t)
	var calls atomic.Int32
	r.probe = func(context.Context, http.RoundTripper) error {
		calls.Add(1)
		return nil
	}

	require.NoError(t, r.Apply(context.Background(), "http://old:1"))
	require.Equal(t, int32(2), calls.Load(), "same-address apply must probe gateway and Codex")
	require.Same(t, oldGateway, r.gateway.Current())
	require.Same(t, oldCodex, r.codex.Current())
	stats := r.Stats().(map[string]any)
	require.Equal(t, "healthy", stats["probe_state"])
	require.Empty(t, stats["probe_error"])
}

func TestProxyRuntimeSameAddressProbeFailureKeepsCurrentRoutes(t *testing.T) {
	r, oldGateway, oldCodex := newRuntimeTest(t)
	r.probe = func(context.Context, http.RoundTripper) error { return context.DeadlineExceeded }

	err := r.Apply(context.Background(), "http://old:1")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Contains(t, err.Error(), "current proxy connectivity check failed")
	require.Same(t, oldGateway, r.gateway.Current())
	require.Same(t, oldCodex, r.codex.Current())
	require.Equal(t, "http://old:1", r.ProxyURL())
	stats := r.Stats().(map[string]any)
	require.Equal(t, "unhealthy", stats["probe_state"])
	require.Contains(t, stats["probe_error"], "context deadline exceeded")
	_, _, lastErr, _ := r.ProxySummary()
	require.Contains(t, lastErr, "context deadline exceeded")
}

func TestProxyRuntimeProbesGatewayAndCodexBeforePublishing(t *testing.T) {
	r, oldGateway, oldCodex := newRuntimeTest(t)
	var seen []string
	r.probe = func(_ context.Context, rt http.RoundTripper) error {
		seen = append(seen, rt.(*runtimeTestRoundTripper).label)
		return nil
	}
	require.NoError(t, r.Apply(context.Background(), "http://new:2"))
	require.Equal(t, []string{"http-gateway", "http-codex-tls-off"}, seen)
	require.NotSame(t, oldGateway, r.gateway.Current())
	require.NotSame(t, oldCodex, r.codex.Current())

	r, oldGateway, oldCodex = newRuntimeTest(t)
	calls := 0
	r.probe = func(_ context.Context, _ http.RoundTripper) error {
		calls++
		if calls == 2 {
			return context.DeadlineExceeded
		}
		return nil
	}
	err := r.Apply(context.Background(), "http://new:2")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Same(t, oldGateway, r.gateway.Current())
	require.Same(t, oldCodex, r.codex.Current())
	_, _, lastErr, _ := r.ProxySummary()
	require.Contains(t, lastErr, "codex")
}

func TestProxyRuntimeConcurrentSwitchAndTLSUpdates(t *testing.T) {
	r, _, _ := newRuntimeTest(t)
	r.probe = func(context.Context, http.RoundTripper) error { return nil }
	const workers = 24
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				if i%2 == 0 {
					_ = r.Apply(context.Background(), "http://one:1")
				} else {
					_ = r.Apply(context.Background(), "http://two:2")
				}
				r.SetTLS((i+j)%2 == 0)
			}
		}(i)
	}
	wg.Wait()
	require.NotNil(t, r.gateway.Current())
	require.NotNil(t, r.codex.Current())
	require.NotEmpty(t, r.ProxyURL())
}

func TestProxyRuntimeRejectsTLSConvergenceBehindProxy(t *testing.T) {
	r, _, _ := newRuntimeTest(t)
	r.proxyFunc = httpx.ProxyFuncs{Scheme: "http", Proxy: func(*http.Request) (*url.URL, error) {
		return nil, nil
	}}
	err := r.SetTLS(true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "TLS convergence")
	require.False(t, r.tls)

	r.tls = true
	err = r.Apply(context.Background(), "http://new:2")
	require.Error(t, err)
	require.Contains(t, err.Error(), "TLS convergence")
	require.Equal(t, "http://old:1", r.ProxyURL())
}

func TestProxyRuntimeTLSProbeMustPassBeforePublishing(t *testing.T) {
	r, _, oldCodex := newRuntimeTest(t)
	r.probe = func(context.Context, http.RoundTripper) error { return context.DeadlineExceeded }
	err := r.SetTLS(true)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, r.tls)
	require.Same(t, oldCodex, r.codex.Current())

	r.probe = nil
	require.NoError(t, r.SetTLS(true))
	require.True(t, r.tls)
	require.NotSame(t, oldCodex, r.codex.Current())
}

func TestProxyRuntimeTLSChangeDoesNotMaskGatewayHealthFailure(t *testing.T) {
	r, _, _ := newRuntimeTest(t)
	r.probe = func(context.Context, http.RoundTripper) error { return nil }
	r.mu.Lock()
	r.probeState = "unhealthy"
	r.probeError = "gateway: connection refused"
	r.probeAt = time.Now().UTC()
	r.mu.Unlock()

	require.NoError(t, r.SetTLS(true))
	r.mu.Lock()
	state := r.probeState
	errText := r.probeError
	r.mu.Unlock()
	require.Equal(t, "unhealthy", state)
	require.Equal(t, "gateway: connection refused", errText)
}

type closeTrackingRoundTripper struct {
	closed atomic.Int32
}

func (r *closeTrackingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
}

func (r *closeTrackingRoundTripper) CloseIdleConnections() { r.closed.Add(1) }

func TestProxyRuntimeTLSPairClosesBothReplacedRoutes(t *testing.T) {
	oldGateway := &closeTrackingRoundTripper{}
	oldCodex := &closeTrackingRoundTripper{}
	gateway, err := httpx.NewSwitchableTransport(oldGateway)
	require.NoError(t, err)
	codex, err := httpx.NewSwitchableTransport(oldCodex)
	require.NoError(t, err)
	r := newProxyTransportRuntime(
		config.UpstreamConfig{}, "", httpx.ProxyFuncs{}, gateway, codex,
		func(httpx.ProxyFuncs) http.RoundTripper { return &closeTrackingRoundTripper{} },
		func(httpx.ProxyFuncs, bool) http.RoundTripper { return &closeTrackingRoundTripper{} },
	)
	r.probe = nil
	require.NoError(t, r.SetTLS(true))
	require.Equal(t, int32(1), oldGateway.closed.Load(), "TLS pair swap must close the replaced gateway pool")
	require.Equal(t, int32(1), oldCodex.closed.Load(), "TLS pair swap must close the replaced Codex pool")
}

func TestProxyRuntimeUnprobedSwitchIsNotReportedHealthy(t *testing.T) {
	r, _, _ := newRuntimeTest(t)
	r.probe = nil
	r.mu.Lock()
	r.probeState = "healthy"
	r.probeAt = time.Now().UTC()
	r.mu.Unlock()

	require.NoError(t, r.Apply(context.Background(), "http://new:2"))
	r.mu.Lock()
	state := r.probeState
	probeAt := r.probeAt
	r.mu.Unlock()
	require.Equal(t, "not_checked", state)
	require.True(t, probeAt.IsZero())
}

func TestProxyRuntimeDirectModeIsReadyWithoutFixedHostProbe(t *testing.T) {
	r, _, _ := newRuntimeTest(t)
	r.proxyURL = ""
	r.probe = func(context.Context, http.RoundTripper) error {
		return errors.New("fixed host unavailable")
	}
	r.StartStartupProbe(context.Background(), nil)
	require.True(t, r.Ready(), "direct mode readiness must not depend on chatgpt.com")
	require.Equal(t, "healthy", r.Stats().(map[string]any)["probe_state"])
}

func TestProxyRuntimeSwitchToDirectSkipsFixedHostProbe(t *testing.T) {
	r, _, _ := newRuntimeTest(t)
	var calls atomic.Int32
	r.probe = func(context.Context, http.RoundTripper) error {
		calls.Add(1)
		return errors.New("fixed host unavailable")
	}

	// Clearing a configured proxy must be a valid route change even when the
	// fixed external probe host is unavailable. Direct mode has no proxy process
	// to validate; readiness is established without network I/O.
	require.NoError(t, r.Apply(context.Background(), ""))
	require.Equal(t, int32(0), calls.Load(), "direct route changes must not probe chatgpt.com")
	require.Empty(t, r.ProxyURL())
	require.True(t, r.Ready())
	require.Equal(t, "healthy", r.Stats().(map[string]any)["probe_state"])
}

func TestProxyRuntimeRetryWorkerRecoversCurrentRoute(t *testing.T) {
	r, oldGateway, oldCodex := newRuntimeTest(t)
	r.retryInterval = time.Millisecond
	var calls atomic.Int32
	r.probe = func(context.Context, http.RoundTripper) error {
		if calls.Add(1) == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}
	// Mark the configured route unhealthy as if the asynchronous startup probe
	// had just failed. The worker must re-probe the same route and leave both
	// published transports untouched.
	r.mu.Lock()
	r.probeState = "unhealthy"
	r.probeError = "context deadline exceeded"
	r.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, r.Start(ctx))
	require.NoError(t, r.Start(ctx), "Start is idempotent")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if r.Ready() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	require.True(t, r.Ready(), "retry worker should publish healthy after probe recovery")
	require.GreaterOrEqual(t, calls.Load(), int32(2))
	require.Same(t, oldGateway, r.gateway.Current())
	require.Same(t, oldCodex, r.codex.Current())
	require.NoError(t, r.Close(context.Background()))
	require.NoError(t, r.Close(context.Background()), "Close is idempotent")
	cancel()
}

func TestProxyRuntimeRetryWorkerStopsOnContextCancel(t *testing.T) {
	r, _, _ := newRuntimeTest(t)
	r.retryInterval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, r.Start(ctx))
	cancel()
	require.NoError(t, r.Close(context.Background()))
	require.NoError(t, r.Close(context.Background()))
	r.workerMu.Lock()
	started := r.workerStarted
	r.workerMu.Unlock()
	require.False(t, started)
}

func TestProbeProxyTransportRejectsRedirectAuthAndServerFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		want bool
	}{
		{name: "policy-forbidden-is-reachable", code: http.StatusForbidden, want: false},
		{name: "redirect", code: http.StatusFound, want: true},
		{name: "proxy-auth", code: http.StatusProxyAuthRequired, want: true},
		{name: "server-failure", code: http.StatusBadGateway, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("fixture"))}, nil
			})
			err := probeProxyTransport(context.Background(), rt)
			if tc.want {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestProxyRuntimeSwitchReachesSharedHTTPClient(t *testing.T) {
	r, _, _ := newRuntimeTest(t)
	r.probe = func(context.Context, http.RoundTripper) error { return nil }
	hc := &http.Client{Transport: r.GatewayTransport()}

	resp, err := hc.Get("http://fixture.invalid")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, "gateway-old", string(body))

	require.NoError(t, r.Apply(context.Background(), "http://new:2"))
	resp, err = hc.Get("http://fixture.invalid")
	require.NoError(t, err)
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, "http-gateway", string(body))

	resp, err = r.CodexTransport().RoundTrip(mustFixtureRequest(t))
	require.NoError(t, err)
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, "http-codex-tls-off", string(body))
}

func TestProxyRuntimeConcurrentTrafficDuringSwitch(t *testing.T) {
	r, _, _ := newRuntimeTest(t)
	r.probe = func(context.Context, http.RoundTripper) error { return nil }
	hc := &http.Client{Transport: r.GatewayTransport()}
	const requesters = 8
	const switches = 40
	errs := make(chan error, 1)
	var wg sync.WaitGroup
	for i := 0; i < requesters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < switches; j++ {
				resp, err := hc.Get("http://fixture.invalid")
				if err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
				body, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil || (string(body) != "gateway-old" && string(body) != "http-gateway") {
					select {
					case errs <- fmt.Errorf("unexpected gateway response %q: %v", body, readErr):
					default:
					}
					return
				}
				resp, err = r.CodexTransport().RoundTrip(mustFixtureRequest(t))
				if err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
				body, readErr = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil || (string(body) != "codex-old" && string(body) != "http-codex-tls-off") {
					select {
					case errs <- fmt.Errorf("unexpected Codex response %q: %v", body, readErr):
					default:
					}
					return
				}
			}
		}(i)
	}
	for i := 0; i < switches; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			route := "http://one:1"
			if i%2 == 1 {
				route = "http://two:2"
			}
			if err := r.Apply(context.Background(), route); err != nil {
				select {
				case errs <- err:
				default:
				}
			}
		}(i)
	}
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
}

func TestProxyRuntimeStartupProbePublishesHealth(t *testing.T) {
	r, _, _ := newRuntimeTest(t)
	started := make(chan struct{}, 2)
	r.probe = func(context.Context, http.RoundTripper) error {
		started <- struct{}{}
		return nil
	}
	r.StartStartupProbe(context.Background(), nil)
	r.mu.Lock()
	done := r.probeDone
	r.mu.Unlock()
	for i := 0; i < 2; i++ {
		<-started
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("startup probe did not publish healthy state")
	}
	stats := r.Stats().(map[string]any)
	require.Equal(t, "healthy", stats["probe_state"])
	require.Empty(t, stats["probe_error"])
}

func TestProxyRuntimeStartupProbeFailureIsObservable(t *testing.T) {
	r, _, _ := newRuntimeTest(t)
	started := make(chan struct{}, 1)
	r.probe = func(context.Context, http.RoundTripper) error {
		select {
		case started <- struct{}{}:
		default:
		}
		return context.DeadlineExceeded
	}
	r.StartStartupProbe(context.Background(), nil)
	r.mu.Lock()
	done := r.probeDone
	r.mu.Unlock()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("startup probe did not start")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("startup probe did not publish unhealthy state")
	}
	r.mu.Lock()
	state := r.probeState
	errText := r.probeError
	r.mu.Unlock()
	require.Equal(t, "unhealthy", state)
	require.Contains(t, errText, "context deadline exceeded")
}

func TestProxyRuntimeStartupProbeStaleResultCannotOverwriteCurrentRoute(t *testing.T) {
	r, _, _ := newRuntimeTest(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	r.probe = func(context.Context, http.RoundTripper) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return nil
	}
	r.StartStartupProbe(context.Background(), nil)
	r.mu.Lock()
	done := r.probeDone
	r.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("startup probe did not start")
	}
	// A route change supersedes the in-flight startup check. Disable probing
	// for Apply so this test only exercises the generation guard.
	r.probe = nil
	r.mu.Lock()
	r.probeState = "checking"
	r.mu.Unlock()
	require.NoError(t, r.Apply(context.Background(), "http://new:2"))
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("startup probe did not finish")
	}
	r.mu.Lock()
	state := r.probeState
	generation := r.generation
	probeAt := r.probeAt
	r.mu.Unlock()
	// The replacement route was deliberately unprobed; the stale result must
	// neither upgrade its health nor stamp it with the old probe completion time.
	require.Equal(t, "not_checked", state)
	require.GreaterOrEqual(t, generation, uint64(1))
	require.True(t, probeAt.IsZero())
}

// TestProxyRuntimeConcurrentApplyUpdatesPreexistingClients exercises the
// startup ordering used by main: the shared gateway client is constructed
// before proxyRuntime replaces the startup wrappers with a pair-backed view.
// Existing clients must still follow subsequent swaps while traffic is in
// flight; otherwise the gateway path silently stays on the startup route even
// though the runtime diagnostics and Codex wrapper report the new route.
func TestProxyRuntimeConcurrentApplyUpdatesPreexistingClients(t *testing.T) {
	initialGateway := &runtimeTestRoundTripper{label: "gateway-old"}
	initialCodex := &runtimeTestRoundTripper{label: "codex-old"}
	startupGateway, err := httpx.NewSwitchableTransport(initialGateway)
	require.NoError(t, err)
	startupCodex, err := httpx.NewSwitchableTransport(initialCodex)
	require.NoError(t, err)

	// These clients intentionally retain the startup wrappers, matching the
	// shared clients captured by aiclient.Factory and Service in main.go.
	sharedGatewayClient := &http.Client{Transport: startupGateway}
	sharedCodexClient := &http.Client{Transport: startupCodex}
	buildGateway := func(f httpx.ProxyFuncs) http.RoundTripper {
		return &runtimeTestRoundTripper{label: f.Scheme + "-gateway"}
	}
	buildCodex := func(f httpx.ProxyFuncs, tls bool) http.RoundTripper {
		return &runtimeTestRoundTripper{label: f.Scheme + "-codex-tls-" + map[bool]string{true: "on", false: "off"}[tls]}
	}
	r := newProxyTransportRuntime(
		config.UpstreamConfig{DialTimeout: 1},
		"http://old:1",
		httpx.ProxyFuncs{Scheme: "http"},
		startupGateway,
		startupCodex,
		buildGateway,
		buildCodex,
	)
	r.probe = func(context.Context, http.RoundTripper) error { return nil }
	// Mirror main.go's composition-root handoff: the existing *http.Client
	// values remain shared, while their transport field is replaced with the
	// stable pair-backed wrappers before requests are accepted.
	sharedGatewayClient.Transport = r.GatewayTransport()
	sharedCodexClient.Transport = r.CodexTransport()

	const workers = 8
	const rounds = 32
	start := make(chan struct{})
	errs := make(chan error, 1)
	var traffic sync.WaitGroup
	for i := 0; i < workers; i++ {
		traffic.Add(1)
		go func() {
			defer traffic.Done()
			<-start
			for j := 0; j < rounds; j++ {
				resp, getErr := sharedGatewayClient.Get("http://fixture.invalid")
				if getErr != nil {
					select {
					case errs <- getErr:
					default:
					}
					return
				}
				body, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil || (string(body) != "gateway-old" && string(body) != "http-gateway") {
					select {
					case errs <- fmt.Errorf("unexpected shared gateway response %q: %v", body, readErr):
					default:
					}
					return
				}

				resp, getErr = sharedCodexClient.Get("http://fixture.invalid")
				if getErr != nil {
					select {
					case errs <- getErr:
					default:
					}
					return
				}
				body, readErr = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil || (string(body) != "codex-old" && string(body) != "http-codex-tls-off") {
					select {
					case errs <- fmt.Errorf("unexpected shared Codex response %q: %v", body, readErr):
					default:
					}
					return
				}
			}
		}()
	}

	var switches sync.WaitGroup
	switches.Add(1)
	go func() {
		defer switches.Done()
		<-start
		for i := 0; i < rounds; i++ {
			route := "http://one:1"
			if i%2 == 1 {
				route = "http://two:2"
			}
			if applyErr := r.Apply(context.Background(), route); applyErr != nil {
				select {
				case errs <- applyErr:
				default:
				}
				return
			}
		}
	}()
	close(start)
	traffic.Wait()
	switches.Wait()
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}

	// Make the final generation deterministic, then verify clients created
	// before pair wrapping observe it too. The Codex wrapper is checked through
	// an actual http.Client rather than Current(), which would only test the
	// newly returned pair view.
	require.NoError(t, r.Apply(context.Background(), "http://new:2"))
	resp, err := sharedGatewayClient.Get("http://fixture.invalid")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, "http-gateway", string(body), "pre-existing shared gateway client must follow runtime swap")

	resp, err = sharedCodexClient.Get("http://fixture.invalid")
	require.NoError(t, err)
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, "http-codex-tls-off", string(body), "pre-existing Codex client must follow runtime swap")
}

func mustFixtureRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://fixture.invalid", nil)
	require.NoError(t, err)
	return req
}
