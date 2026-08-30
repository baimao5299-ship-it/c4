// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/pkg/httpx"
	"github.com/is7qin/c3api/pkg/logx"
)

// proxyTransportRuntime owns the two transport families used by c3api. The
// gateway and Codex clients each keep a stable SwitchableTransport pointer;
// changing a proxy therefore never requires rebuilding clients or restarting
// the server.
type proxyTransportRuntime struct {
	// mu protects the published route and diagnostic snapshot. It is held only
	// for short state reads/writes; network probes never run under this lock.
	mu sync.RWMutex
	// switchMu serializes route decisions. Keeping it separate from mu means a
	// slow or unreachable proxy cannot block /ops/workers or settings reads.
	switchMu sync.Mutex

	cfg     config.UpstreamConfig
	gateway *httpx.SwitchableTransport
	codex   *httpx.SwitchableTransport
	pair    *httpx.TransportPair

	buildGateway func(httpx.ProxyFuncs) http.RoundTripper
	buildCodex   func(httpx.ProxyFuncs, bool) http.RoundTripper
	probe        func(context.Context, http.RoundTripper) error

	proxyURL  string
	proxyFunc httpx.ProxyFuncs
	tls       bool
	lastError string
	lastAt    time.Time
	// generation changes whenever a new route is published. Startup probes run
	// in the background, so a result from an older route must never make the
	// current route look healthy after a concurrent switch.
	generation uint64
	probeState string
	probeError string
	probeAt    time.Time
	probeDone  chan struct{}
	// probeSeq invalidates overlapping health checks that target the same
	// route generation (for example, startup check racing an explicit
	// same-address re-apply). Route generation alone cannot distinguish those
	// checks because the address and transport remain unchanged.
	probeSeq      uint64
	workerMu      sync.Mutex
	workerStarted bool
	workerCancel  context.CancelFunc
	workerDone    chan struct{}
	retryInterval time.Duration
}

// proxyRouteSnapshot identifies the route a background recovery attempt read.
// The proxy URL alone is not enough: re-applying the same address is a valid
// operator action and increments generation even though the string is unchanged.
type proxyRouteSnapshot struct {
	proxyURL   string
	generation uint64
}

const proxyRetryInterval = 30 * time.Second

func newProxyTransportRuntime(cfg config.UpstreamConfig, proxyURL string, proxyFuncs httpx.ProxyFuncs, gateway, codex *httpx.SwitchableTransport, buildGateway func(httpx.ProxyFuncs) http.RoundTripper, buildCodex func(httpx.ProxyFuncs, bool) http.RoundTripper) *proxyTransportRuntime {
	// Publish the initial gateway/Codex routes as one pair. The incoming
	// transports are startup-only wrappers; callers must use the returned
	// runtime transports after construction (see main.go).
	var pair *httpx.TransportPair
	if gateway != nil && codex != nil {
		if p, pairedGateway, pairedCodex, err := httpx.NewTransportPair(gateway.Current(), codex.Current()); err == nil {
			// Keep the pair so Apply/SetTLS can publish both routes together.
			pair = p
			gateway, codex = pairedGateway, pairedCodex
		}
	}
	return &proxyTransportRuntime{
		cfg:           cfg,
		proxyURL:      proxyURL,
		proxyFunc:     proxyFuncs,
		tls:           cfg.TLSConvergenceEnabled,
		gateway:       gateway,
		codex:         codex,
		pair:          pair,
		buildGateway:  buildGateway,
		buildCodex:    buildCodex,
		probe:         probeProxyTransport,
		probeState:    "not_checked",
		retryInterval: proxyRetryInterval,
	}
}

func (r *proxyTransportRuntime) GatewayTransport() *httpx.SwitchableTransport {
	if r == nil {
		return nil
	}
	return r.gateway
}

func (r *proxyTransportRuntime) CodexTransport() *httpx.SwitchableTransport {
	if r == nil {
		return nil
	}
	return r.codex
}

// Apply parses and probes a candidate route before publishing it. The mutex
// serializes switch decisions, while request execution remains lock-free in
// SwitchableTransport. A failed candidate leaves both existing routes intact.
func (r *proxyTransportRuntime) Apply(ctx context.Context, raw string) error {
	return r.apply(ctx, raw, nil)
}

// apply is the serialized route switch implementation. expected is supplied
// only by the retry worker; it prevents a stale tick from restoring an older
// proxy after an operator has already selected a newer route.
func (r *proxyTransportRuntime) apply(ctx context.Context, raw string, expected *proxyRouteSnapshot) error {
	if r == nil {
		return fmt.Errorf("proxy runtime is not configured")
	}
	raw = strings.TrimSpace(raw)
	if ctx == nil {
		ctx = context.Background()
	}
	funcs, err := httpx.ParseProxyWithTimeout(raw, r.cfg.DialTimeout)
	if err != nil {
		r.mu.Lock()
		r.lastError = err.Error()
		r.lastAt = time.Now().UTC()
		r.mu.Unlock()
		return err
	}
	r.switchMu.Lock()
	defer r.switchMu.Unlock()
	r.mu.RLock()
	currentURL := r.proxyURL
	currentGeneration := r.generation
	currentProbeState := r.probeState
	tls := r.tls
	buildGateway := r.buildGateway
	buildCodex := r.buildCodex
	gateway := r.gateway
	codex := r.codex
	probe := r.probe
	pair := r.pair
	r.mu.RUnlock()
	if expected != nil && (expected.proxyURL != currentURL || expected.generation != currentGeneration) {
		// The operator won the race while the worker was waiting for the switch
		// mutex. Keep the newly selected route and let the next tick observe it.
		return nil
	}
	if expected != nil && currentProbeState == "healthy" {
		// A foreground apply or another retry already recovered this route.
		return nil
	}
	if raw == currentURL {
		// Direct mode has no proxy process to probe. Re-applying an empty route
		// must not make readiness depend on a fixed external host (the same rule
		// used by StartStartupProbe); this is also important when an operator
		// clears a stale proxy override after the proxy process has gone away.
		if raw == "" {
			now := time.Now().UTC()
			r.mu.Lock()
			r.probeState = "healthy"
			r.probeError = ""
			r.probeAt = now
			r.lastError = ""
			r.lastAt = now
			r.mu.Unlock()
			return nil
		}
		// Re-apply of the same address is still a meaningful operator action:
		// the proxy process or its selected node may have recovered/changed
		// while the URL stayed constant. Re-probe the already-published routes
		// instead of treating the address string as a health guarantee. The
		// switch mutex keeps Apply/SetTLS from replacing either route while the
		// bounded probe is in flight; request traffic remains lock-free.
		if probe != nil {
			if gateway == nil || codex == nil {
				err := fmt.Errorf("proxy transports are not configured")
				r.mu.Lock()
				r.lastError = err.Error()
				r.lastAt = time.Now().UTC()
				r.probeState = "unhealthy"
				r.probeError = err.Error()
				r.probeAt = r.lastAt
				r.mu.Unlock()
				return fmt.Errorf("current proxy connectivity check failed: %w", err)
			}
			probeCtx, cancel := context.WithTimeout(ctx, 16*time.Second)
			defer cancel()
			r.mu.Lock()
			generation := r.generation
			r.probeSeq++
			probeID := r.probeSeq
			r.probeState = "checking"
			r.probeError = ""
			r.probeAt = time.Now().UTC()
			r.mu.Unlock()
			probeErr := runProxyProbe(probeCtx, probe, gateway)
			if probeErr == nil {
				probeErr = runProxyProbe(probeCtx, probe, codex)
			}
			now := time.Now().UTC()
			r.mu.Lock()
			// A startup probe may have raced this operation. Do not overwrite a
			// newer route's state if the generation changed while probing.
			if generation == r.generation && probeID == r.probeSeq {
				r.probeAt = now
				if probeErr != nil {
					r.probeState = "unhealthy"
					r.probeError = probeErr.Error()
					r.lastError = probeErr.Error()
					r.lastAt = now
				} else {
					r.probeState = "healthy"
					r.probeError = ""
					r.lastError = ""
					r.lastAt = now
				}
			}
			r.mu.Unlock()
			if probeErr != nil {
				return fmt.Errorf("current proxy connectivity check failed: %w", probeErr)
			}
			return nil
		}
		// Test/integration runtimes may intentionally omit a probe hook. Keep
		// the historical no-op behavior, but do not claim a fresh health result.
		r.mu.Lock()
		r.lastError = ""
		r.mu.Unlock()
		return nil
	}
	if buildGateway == nil || buildCodex == nil {
		r.mu.Lock()
		r.lastError = "proxy transport builders are not configured"
		r.lastAt = time.Now().UTC()
		r.mu.Unlock()
		return fmt.Errorf("proxy transport builders are not configured")
	}
	if gateway == nil || codex == nil {
		r.mu.Lock()
		r.lastError = "proxy transports are not configured"
		r.lastAt = time.Now().UTC()
		r.mu.Unlock()
		return fmt.Errorf("proxy transports are not configured")
	}
	if tls && (funcs.Proxy != nil || funcs.DialContext != nil) {
		r.mu.Lock()
		r.lastError = "TLS convergence requires direct upstream transport"
		r.lastAt = time.Now().UTC()
		r.mu.Unlock()
		return fmt.Errorf("TLS convergence requires direct upstream transport")
	}
	candidateGateway := buildGateway(funcs)
	if httpx.IsNilRoundTripper(candidateGateway) {
		r.mu.Lock()
		r.lastError = "gateway transport builder returned nil"
		r.lastAt = time.Now().UTC()
		r.mu.Unlock()
		return fmt.Errorf("gateway transport builder returned nil")
	}
	candidateCodex := buildCodex(funcs, tls)
	if httpx.IsNilRoundTripper(candidateCodex) {
		httpx.CloseIdle(candidateGateway)
		r.mu.Lock()
		r.lastError = "codex transport builder returned nil"
		r.lastAt = time.Now().UTC()
		r.mu.Unlock()
		return fmt.Errorf("codex transport builder returned nil")
	}
	// Direct mode is intentionally considered healthy without probing a fixed
	// provider host. The actual upstream endpoint probes remain available from
	// the management surface, while proxy routes still require transport-level
	// validation before publication.
	validated := probe != nil && raw != ""
	if validated {
		probeCtx, cancel := context.WithTimeout(ctx, 16*time.Second)
		defer cancel()
		if err := runProxyProbe(probeCtx, probe, candidateGateway); err != nil {
			httpx.CloseIdle(candidateGateway)
			httpx.CloseIdle(candidateCodex)
			r.mu.Lock()
			r.lastError = "gateway: " + err.Error()
			r.lastAt = time.Now().UTC()
			r.mu.Unlock()
			return fmt.Errorf("gateway proxy connectivity check failed: %w", err)
		}
		if err := runProxyProbe(probeCtx, probe, candidateCodex); err != nil {
			httpx.CloseIdle(candidateGateway)
			httpx.CloseIdle(candidateCodex)
			r.mu.Lock()
			r.lastError = "codex: " + err.Error()
			r.lastAt = time.Now().UTC()
			r.mu.Unlock()
			return fmt.Errorf("codex proxy connectivity check failed: %w", err)
		}
	}
	// The pair-backed wrappers publish both routes in one atomic snapshot.
	// Standalone wrappers remain supported for focused tests/integrations.
	var oldGateway, oldCodex http.RoundTripper
	r.mu.Lock()
	if pair != nil {
		oldGateway, oldCodex = pair.Swap(candidateGateway, candidateCodex)
	} else {
		oldGateway = gateway.Swap(candidateGateway)
		oldCodex = codex.Swap(candidateCodex)
	}
	r.proxyURL = raw
	r.proxyFunc = funcs
	r.lastError = ""
	r.lastAt = time.Now().UTC()
	r.generation++
	// Invalidate any startup/retry probe that captured the previous route.
	r.probeSeq++
	// Apply probes both candidate routes before publishing, so the newly
	// selected route is already known healthy. If probing is unavailable, make
	// that explicit rather than claiming health from an unvalidated transport.
	if validated || raw == "" {
		r.probeState = "healthy"
		r.probeError = ""
		r.probeAt = r.lastAt
	} else {
		r.probeState = "not_checked"
		r.probeError = ""
		r.probeAt = time.Time{}
	}
	r.mu.Unlock()
	httpx.CloseIdle(oldGateway)
	httpx.CloseIdle(oldCodex)
	return nil
}

// SetTLS changes only the Codex TLS profile while preserving the selected
// proxy. It uses the same atomic replacement as proxy changes.
func (r *proxyTransportRuntime) SetTLS(enabled bool) error {
	if r == nil {
		return fmt.Errorf("proxy runtime is not configured")
	}
	r.switchMu.Lock()
	defer r.switchMu.Unlock()
	r.mu.RLock()
	currentTLS := r.tls
	buildCodex := r.buildCodex
	codex := r.codex
	pair := r.pair
	gateway := r.gateway
	proxyFuncs := r.proxyFunc
	probe := r.probe
	r.mu.RUnlock()
	if currentTLS == enabled {
		return nil
	}
	if buildCodex == nil || codex == nil {
		r.mu.Lock()
		r.lastError = "codex transport builder or transport is not configured"
		r.lastAt = time.Now().UTC()
		r.mu.Unlock()
		return fmt.Errorf("codex transport builder or transport is not configured")
	}
	if enabled && (proxyFuncs.Proxy != nil || proxyFuncs.DialContext != nil) {
		r.mu.Lock()
		r.lastError = "TLS convergence requires direct upstream transport"
		r.lastAt = time.Now().UTC()
		r.mu.Unlock()
		return fmt.Errorf("TLS convergence requires direct upstream transport")
	}
	next := buildCodex(proxyFuncs, enabled)
	if httpx.IsNilRoundTripper(next) {
		r.mu.Lock()
		r.lastError = "codex transport builder returned nil"
		r.lastAt = time.Now().UTC()
		r.mu.Unlock()
		return fmt.Errorf("codex transport builder returned nil")
	}
	validated := probe != nil
	if validated {
		probeCtx, cancel := context.WithTimeout(context.Background(), 16*time.Second)
		defer cancel()
		if err := runProxyProbe(probeCtx, probe, next); err != nil {
			httpx.CloseIdle(next)
			r.mu.Lock()
			r.lastError = "codex TLS: " + err.Error()
			r.lastAt = time.Now().UTC()
			r.mu.Unlock()
			return fmt.Errorf("codex TLS connectivity check failed: %w", err)
		}
	}
	var oldGateway, oldCodex http.RoundTripper
	r.mu.RLock()
	previousProbeState := r.probeState
	r.mu.RUnlock()
	r.mu.Lock()
	if pair != nil {
		currentGateway := gateway.Current()
		if currentGateway == nil {
			r.lastError = "gateway transport has no current route"
			r.lastAt = time.Now().UTC()
			r.mu.Unlock()
			httpx.CloseIdle(next)
			return fmt.Errorf("gateway transport has no current route")
		}
		oldGateway, oldCodex = pair.Swap(currentGateway, next)
	} else {
		oldCodex = codex.Swap(next)
	}
	r.tls = enabled
	r.lastAt = time.Now().UTC()
	r.generation++
	// A TLS replacement invalidates probes that captured the old Codex route.
	// The gateway route is unchanged, so preserve a known unhealthy state; a
	// probe that was still running becomes not_checked and will be retried by
	// the runtime worker instead of being reported as superseded forever.
	r.probeSeq++
	// SetTLS validates only the replacement Codex transport. Preserve the
	// existing full-route health state instead of claiming the gateway is
	// healthy without probing it.
	if !validated || previousProbeState == "checking" {
		r.probeState = "not_checked"
		r.probeError = ""
		r.probeAt = time.Time{}
	} else if validated && previousProbeState == "healthy" {
		r.probeState = "healthy"
		r.probeError = ""
		r.probeAt = r.lastAt
	}
	r.mu.Unlock()
	// A TLS-only update still republishes the pair atomically. Close both
	// replaced routes; retaining the gateway's idle pool on every toggle would
	// leak sockets until the process exits.
	httpx.CloseIdle(oldGateway)
	httpx.CloseIdle(oldCodex)
	return nil
}

// Start runs a low-frequency recovery probe for an unhealthy configured proxy.
// A transient outage during process startup must not leave /readyz stuck at
// 503 forever; retries only re-probe the currently selected route and never
// replace it automatically. The worker is idempotent and non-blocking.
func (r *proxyTransportRuntime) Start(ctx context.Context) error {
	if r == nil {
		return fmt.Errorf("proxy runtime is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.workerMu.Lock()
	if r.workerStarted {
		r.workerMu.Unlock()
		return nil
	}
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	r.workerStarted = true
	r.workerCancel = cancel
	r.workerDone = done
	r.workerMu.Unlock()
	go func() {
		defer close(done)
		interval := r.retryInterval
		if interval <= 0 {
			interval = proxyRetryInterval
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				r.mu.RLock()
				snapshot := proxyRouteSnapshot{proxyURL: r.proxyURL, generation: r.generation}
				state := r.probeState
				r.mu.RUnlock()
				if snapshot.proxyURL == "" || state == "healthy" || state == "checking" {
					continue
				}
				if err := r.apply(workerCtx, snapshot.proxyURL, &snapshot); err != nil {
					// Apply records the bounded error in the runtime snapshot. Do
					// not log every retry here; the management surface remains the
					// single stable diagnostic channel during a proxy outage.
					continue
				}
			}
		}
	}()
	return nil
}

// Close stops the retry worker. It never changes the selected route and is
// safe before Start or when called more than once.
func (r *proxyTransportRuntime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.workerMu.Lock()
	if !r.workerStarted {
		r.workerMu.Unlock()
		return nil
	}
	cancel := r.workerCancel
	done := r.workerDone
	r.workerStarted = false
	r.workerCancel = nil
	r.workerDone = nil
	r.workerMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// StartStartupProbe verifies the configured startup route asynchronously. It
// deliberately does not alter the route: a failed check is reported through
// the runtime stats and logs, while existing traffic remains untouched. A
// generation check prevents a slow probe for an old route from overwriting
// the status of a newer route selected concurrently.
func (r *proxyTransportRuntime) StartStartupProbe(parent context.Context, log *logx.Logger) {
	if r == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	r.mu.Lock()
	if r.probeState == "checking" {
		r.mu.Unlock()
		return
	}
	// Direct mode intentionally has no proxy process whose health can be
	// inferred from a fixed external host.  A deployment may serve a private
	// or OpenAI-compatible upstream that does not depend on chatgpt.com, so
	// treating that host as a readiness gate would keep an otherwise usable
	// instance out of rotation.  The upstream's own probe/test endpoints remain
	// available from the management surface; only configured proxy routes need
	// this transport-level check.
	if strings.TrimSpace(r.proxyURL) == "" {
		r.probeState = "healthy"
		r.probeError = ""
		r.probeAt = time.Now().UTC()
		r.lastError = ""
		r.lastAt = r.probeAt
		r.mu.Unlock()
		return
	}
	// Apply/SetTLS already validated a newly published route. Do not probe the
	// same route a second time during startup (important for rate-limited
	// proxies); the initial config path remains not_checked and is still tested.
	if r.probeState == "healthy" && !r.probeAt.IsZero() {
		r.mu.Unlock()
		return
	}
	generation := r.generation
	r.probeSeq++
	probeID := r.probeSeq
	gateway := r.gateway
	codex := r.codex
	probe := r.probe
	r.probeState = "checking"
	r.probeError = ""
	r.probeAt = time.Now().UTC()
	done := make(chan struct{})
	r.probeDone = done
	r.mu.Unlock()

	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(parent, 16*time.Second)
		defer cancel()
		var err error
		if gateway == nil || codex == nil {
			err = fmt.Errorf("proxy transports are not configured")
		} else if probe == nil {
			err = fmt.Errorf("proxy health probe is not configured")
		} else {
			if err = runProxyProbe(ctx, probe, gateway); err == nil {
				err = runProxyProbe(ctx, probe, codex)
			}
		}

		r.mu.Lock()
		defer r.mu.Unlock()
		if generation != r.generation || probeID != r.probeSeq {
			// A successful Apply/SetTLS has already published a newer validated
			// route. Preserve that result; only expose superseded when no newer
			// health result exists to describe the current route.
			if generation != r.generation && r.probeState == "checking" {
				r.probeState = "superseded"
				r.probeError = "route changed while startup check was running"
			}
			return
		}
		// Only a result for the current route generation may update the
		// timestamp exposed to operators. Stale probes must not make a newer
		// unvalidated route look recently checked.
		r.probeAt = time.Now().UTC()
		if err != nil {
			r.probeState = "unhealthy"
			r.probeError = err.Error()
			if log != nil {
				log.Warn("startup proxy health check failed", logx.Error(err))
			}
			return
		}
		r.probeState = "healthy"
		r.probeError = ""
		if log != nil {
			log.Info("startup proxy health check passed")
		}
	}()
}

// ProxyURL returns the selected URL for diagnostics. Callers should use
// ProxySummary for user-facing output so credentials never leave the process.
func (r *proxyTransportRuntime) ProxyURL() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.proxyURL
}

func (r *proxyTransportRuntime) ProxySummary() (string, bool, string, time.Time) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return httpx.ProxySummary(r.proxyURL), r.proxyURL != "", r.lastError, r.lastAt
}

// Ready reports whether the currently published proxy/Codex routes completed
// a successful transport probe. It is intentionally stricter than liveness:
// the admin console remains reachable while an unhealthy proxy is diagnosed,
// but a public load balancer should keep this instance out of rotation.
func (r *proxyTransportRuntime) Ready() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.probeState == "healthy" && r.probeError == ""
}

// Name and Stats implement the management worker observation contract without
// making the hot request path depend on the admin handler package.
func (r *proxyTransportRuntime) Name() string { return "proxy-runtime" }

func (r *proxyTransportRuntime) Stats() any {
	if r == nil {
		return map[string]any{"state": "unconfigured"}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return map[string]any{
		"proxy":         httpx.ProxySummary(r.proxyURL),
		"configured":    r.proxyURL != "",
		"tls_converged": r.tls,
		"probe_state":   r.probeState,
		"probe_error":   r.probeError,
		"probe_at":      r.probeAt,
		"last_error":    r.lastError,
		"last_error_at": r.lastAt,
		"generation":    r.generation,
	}
}

// probeProxyTransport verifies the transport and TLS path without requiring
// upstream authorization. Normal policy responses such as 401/403/429 still
// prove that the route reached the service; redirects, proxy-auth failures and
// upstream 5xx responses are rejected because they commonly indicate a captive
// portal, a bad proxy credential, or a broken relay.
func probeProxyTransport(parent context.Context, rt http.RoundTripper) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/", nil)
	if err != nil {
		return err
	}
	client := &http.Client{
		Transport: rt,
		// A redirect can hide a captive portal or move the probe to a host that
		// the configured proxy was never meant to reach. The first response is
		// enough to verify the transport itself.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 1<<20)
	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		return fmt.Errorf("proxy probe redirected (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusProxyAuthRequired {
		return fmt.Errorf("proxy authentication required (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("proxy probe upstream failure (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// runProxyProbe converts a faulty probe implementation into a normal update
// error. The production probe is local code, but keeping this boundary
// panic-safe prevents a future adapter or test hook from taking down the
// process during a diagnostics operation.
func runProxyProbe(ctx context.Context, probe func(context.Context, http.RoundTripper) error, rt http.RoundTripper) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if probe == nil {
		return fmt.Errorf("proxy health probe is not configured")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("proxy health probe panicked: %v", recovered)
		}
	}()
	return probe(ctx, rt)
}
