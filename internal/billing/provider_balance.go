// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// This file contains the provider-balance building block used by management and
// scheduling surfaces. It deliberately has no repository or scheduler dependency:
// providers can be wired in later without making an unsupported balance endpoint look
// like a zero balance.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type BalanceStatus string

const (
	BalanceStatusFresh        BalanceStatus = "fresh"
	BalanceStatusStale        BalanceStatus = "stale"
	BalanceStatusUnavailable  BalanceStatus = "unavailable"
	BalanceStatusUnconfigured BalanceStatus = "unconfigured"
)

type BalanceErrorCode string

const (
	BalanceErrorNoEndpoint   BalanceErrorCode = "no_endpoint"
	BalanceErrorUpstream     BalanceErrorCode = "upstream"
	BalanceErrorAuth         BalanceErrorCode = "auth"
	BalanceErrorRateLimited  BalanceErrorCode = "rate_limited"
	BalanceErrorInvalidValue BalanceErrorCode = "invalid_value"
	BalanceErrorCanceled     BalanceErrorCode = "context_canceled"
	BalanceErrorTimeout      BalanceErrorCode = "timeout"
)

// BalanceAccount is the non-secret context passed to an adapter. UpstreamKey is
// never copied into a Snapshot or an error message.
type BalanceAccount struct {
	ID          int64
	Provider    string
	BaseURL     string
	UpstreamKey string
}

// ProviderBalance is the successful result returned by an adapter. Amount stays a
// string so the management UI does not lose decimal precision while the cache uses
// exact rational comparison for its low-balance threshold.
type ProviderBalance struct {
	Amount   string
	Currency string
}

// BalanceAdapter is intentionally small so a provider can implement its own
// authentication and response mapping. EndpointConfigured must be false when the
// provider has no balance API; callers then receive "unconfigured", never balance 0.
type BalanceAdapter interface {
	Provider() string
	EndpointConfigured() bool
	Fetch(context.Context, BalanceAccount) (ProviderBalance, error)
}

// BalanceAdapterRegistry owns the immutable provider-name mapping used by
// management wiring. Registration is explicit and rejects ambiguous providers;
// a missing provider remains a normal unconfigured result at the cache boundary.
type BalanceAdapterRegistry struct {
	mu       sync.RWMutex
	adapters map[string]BalanceAdapter
}

type balanceAdapterValidator interface{ Validate() error }

func NewBalanceAdapterRegistry(adapters ...BalanceAdapter) (*BalanceAdapterRegistry, error) {
	r := &BalanceAdapterRegistry{adapters: make(map[string]BalanceAdapter, len(adapters))}
	for _, adapter := range adapters {
		if isNilBalanceAdapter(adapter) {
			return nil, errors.New("balance adapter must not be nil")
		}
		name := strings.TrimSpace(adapter.Provider())
		if name == "" {
			return nil, errors.New("balance adapter provider must not be empty")
		}
		if _, exists := r.adapters[name]; exists {
			return nil, fmt.Errorf("duplicate balance adapter provider %q", name)
		}
		if validator, ok := adapter.(balanceAdapterValidator); ok {
			if err := validator.Validate(); err != nil {
				return nil, fmt.Errorf("balance adapter %q: %w", name, err)
			}
		}
		r.adapters[name] = adapter
	}
	return r, nil
}

func isNilBalanceAdapter(adapter BalanceAdapter) bool {
	if adapter == nil {
		return true
	}
	v := reflect.ValueOf(adapter)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func (r *BalanceAdapterRegistry) Get(provider string) (BalanceAdapter, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	adapter, ok := r.adapters[strings.TrimSpace(provider)]
	r.mu.RUnlock()
	return adapter, ok
}

func (r *BalanceAdapterRegistry) Providers() []string {
	if r == nil {
		return []string{}
	}
	r.mu.RLock()
	out := make([]string, 0, len(r.adapters))
	for name := range r.adapters {
		out = append(out, name)
	}
	r.mu.RUnlock()
	slices.Sort(out)
	return out
}

// BalanceSnapshot is safe to expose to a control plane. ErrorCode is a bounded class,
// not upstream response text. CheckedAt is the last successful provider read;
// AttemptedAt records the most recent attempt, including failed refreshes.
type BalanceSnapshot struct {
	AccountID   int64            `json:"account_id"`
	Provider    string           `json:"provider"`
	Status      BalanceStatus    `json:"status"`
	Amount      string           `json:"amount,omitempty"`
	Currency    string           `json:"currency,omitempty"`
	Low         bool             `json:"low"`
	CheckedAt   time.Time        `json:"checked_at,omitempty"`
	AttemptedAt time.Time        `json:"attempted_at,omitempty"`
	ExpiresAt   time.Time        `json:"expires_at,omitempty"`
	StaleUntil  time.Time        `json:"stale_until,omitempty"`
	ErrorCode   BalanceErrorCode `json:"error_code,omitempty"`
}

// BalanceCacheConfig controls freshness and stale-if-error behavior. A stale value is
// retained only for StaleIfError after TTL; no failed request can turn an account into
// a zero balance.
type BalanceCacheConfig struct {
	TTL                 time.Duration
	StaleIfError        time.Duration
	FailureCooldown     time.Duration
	MaxEntries          int
	LowBalanceThreshold string
	Now                 func() time.Time
}

// BalanceCache is a small per-account cache with request coalescing. It is intended
// for low-frequency management/health reads, not the request hot path.
type BalanceCache struct {
	mu         sync.Mutex
	entries    map[string]*balanceCacheEntry
	ttl        time.Duration
	staleFor   time.Duration
	failureFor time.Duration
	maxEntries int
	threshold  *big.Rat
	now        func() time.Time
}

type balanceCacheEntry struct {
	accountID int64
	provider  string
	snapshot  BalanceSnapshot
	fetching  bool
	wait      chan struct{}
	retryAt   time.Time
	lastUsed  time.Time
}

func NewBalanceCache(cfg BalanceCacheConfig) (*BalanceCache, error) {
	if cfg.TTL <= 0 {
		return nil, errors.New("balance cache ttl must be positive")
	}
	if cfg.StaleIfError < 0 {
		return nil, errors.New("balance cache stale_if_error must be non-negative")
	}
	if cfg.FailureCooldown < 0 {
		return nil, errors.New("balance cache failure_cooldown must be non-negative")
	}
	maxEntries := cfg.MaxEntries
	if maxEntries == 0 {
		maxEntries = 4096
	}
	if maxEntries < 1 {
		return nil, errors.New("balance cache max_entries must be positive")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	var threshold *big.Rat
	if strings.TrimSpace(cfg.LowBalanceThreshold) != "" {
		var ok bool
		threshold, ok = parseAmount(cfg.LowBalanceThreshold)
		if !ok || threshold.Sign() < 0 {
			return nil, errors.New("balance cache low balance threshold must be a non-negative number")
		}
	}
	return &BalanceCache{
		entries:    make(map[string]*balanceCacheEntry),
		ttl:        cfg.TTL,
		staleFor:   cfg.StaleIfError,
		failureFor: cfg.FailureCooldown,
		maxEntries: maxEntries,
		threshold:  threshold,
		now:        now,
	}, nil
}

// Get returns a fresh snapshot, a bounded stale snapshot on refresh failure, or an
// explicit unavailable/unconfigured state. Concurrent refreshes for the same provider
// and account collapse to one adapter call.
func (c *BalanceCache) Get(ctx context.Context, account BalanceAccount, adapter BalanceAdapter) BalanceSnapshot {
	if c == nil || c.now == nil || c.ttl <= 0 {
		return unavailableSnapshot(account.ID, strings.TrimSpace(account.Provider), time.Now(), BalanceErrorNoEndpoint)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if isNilBalanceAdapter(adapter) {
		adapter = nil
	}
	provider := account.Provider
	if adapter != nil && strings.TrimSpace(provider) == "" {
		provider = adapter.Provider()
	}
	provider = strings.TrimSpace(provider)
	key := balanceCacheKey(provider, account, adapter)
	for {
		now := c.now()
		c.mu.Lock()
		e := c.entries[key]
		if e == nil {
			if len(c.entries) >= c.maxEntries && !c.evictOneLocked(key) {
				c.mu.Unlock()
				return c.fetchWithoutCache(ctx, account, provider, adapter)
			}
			e = &balanceCacheEntry{accountID: account.ID, provider: provider}
			c.entries[key] = e
		}
		e.lastUsed = now
		if e.snapshot.Status == BalanceStatusFresh && now.Before(e.snapshot.ExpiresAt) {
			s := e.snapshot
			c.mu.Unlock()
			return s
		}
		if e.fetching {
			wait := e.wait
			c.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return unavailableSnapshot(account.ID, provider, now, errorCode(ctx.Err()))
			}
		}
		if !e.retryAt.IsZero() && now.Before(e.retryAt) {
			s := e.snapshot
			if s.Status == BalanceStatusStale && !now.Before(s.StaleUntil) {
				s = unavailableSnapshot(account.ID, provider, now, s.ErrorCode)
			}
			if s.Status == "" {
				s = unavailableSnapshot(account.ID, provider, now, BalanceErrorUpstream)
			}
			c.mu.Unlock()
			return s
		}
		if adapter == nil || !adapter.EndpointConfigured() {
			e.snapshot = BalanceSnapshot{AccountID: account.ID, Provider: provider, Status: BalanceStatusUnconfigured, AttemptedAt: now, ErrorCode: BalanceErrorNoEndpoint}
			e.retryAt = now.Add(c.failureFor)
			s := e.snapshot
			c.mu.Unlock()
			return s
		}
		// Claim the refresh before releasing the lock. A waiter retries the loop after
		// the channel closes and observes either the new value or the stale result.
		e.fetching = true
		e.wait = make(chan struct{})
		wait := e.wait
		previous := e.snapshot
		c.mu.Unlock()

		result, err := adapter.Fetch(ctx, account)
		now = c.now()
		c.mu.Lock()
		e.fetching = false
		close(wait)
		if ctx.Err() != nil {
			// Caller cancellation is not a provider failure. Keep the last successful
			// value intact so a cancelled dashboard request cannot erase a usable cache.
			e.snapshot = previous
			e.retryAt = time.Time{}
			s := unavailableSnapshot(account.ID, provider, now, errorCode(ctx.Err()))
			c.mu.Unlock()
			return s
		} else if err == nil {
			var validationErr error
			e.snapshot, validationErr = c.successSnapshot(account.ID, provider, result, now)
			if validationErr != nil {
				e.retryAt = now.Add(c.failureFor)
				if stale, ok := c.staleSnapshot(previous, now, validationErr); ok {
					e.snapshot = stale
				}
			} else {
				e.retryAt = time.Time{}
			}
		} else if stale, ok := c.staleSnapshot(previous, now, err); ok {
			e.snapshot = stale
			e.retryAt = now.Add(c.failureFor)
		} else {
			e.snapshot = unavailableSnapshot(account.ID, provider, now, classifyBalanceError(err))
			e.retryAt = now.Add(c.failureFor)
		}
		s := e.snapshot
		c.mu.Unlock()
		return s
	}
}

// Invalidate removes every cached variant of an account. It is used after key,
// endpoint, or provider settings change; in-flight fetches are allowed to finish
// but their result is no longer addressable by a future cache lookup.
func (c *BalanceCache) Invalidate(provider string, accountID int64) {
	if c == nil {
		return
	}
	provider = strings.TrimSpace(provider)
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		if entry.provider == provider && entry.accountID == accountID {
			delete(c.entries, key)
		}
	}
}

// InvalidateAccount clears every provider/configuration variant for an account.
// It is the preferred path after an account edit because it does not require a
// second database read while the management mutation is finishing.
func (c *BalanceCache) InvalidateAccount(accountID int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		if entry.accountID == accountID {
			delete(c.entries, key)
		}
	}
}

func (c *BalanceCache) evictOneLocked(exclude string) bool {
	if len(c.entries) < c.maxEntries {
		return true
	}
	var oldestKey string
	var oldest time.Time
	for key, entry := range c.entries {
		if key == exclude || entry.fetching {
			continue
		}
		if oldestKey == "" || entry.lastUsed.Before(oldest) {
			oldestKey, oldest = key, entry.lastUsed
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
		return true
	}
	return false
}

// fetchWithoutCache handles a transient full-cache condition when every
// resident entry is already fetching. It preserves MaxEntries as a hard bound;
// the caller still receives a useful point-in-time result, but that result is
// deliberately not retained because there is no slot to coalesce it safely.
func (c *BalanceCache) fetchWithoutCache(ctx context.Context, account BalanceAccount, provider string, adapter BalanceAdapter) BalanceSnapshot {
	now := c.now()
	if isNilBalanceAdapter(adapter) {
		adapter = nil
	}
	if adapter == nil || !adapter.EndpointConfigured() {
		return unavailableSnapshot(account.ID, provider, now, BalanceErrorNoEndpoint)
	}
	result, err := adapter.Fetch(ctx, account)
	now = c.now()
	if ctx.Err() != nil {
		return unavailableSnapshot(account.ID, provider, now, errorCode(ctx.Err()))
	}
	if err != nil {
		return unavailableSnapshot(account.ID, provider, now, classifyBalanceError(err))
	}
	snapshot, validationErr := c.successSnapshot(account.ID, provider, result, now)
	if validationErr != nil {
		return unavailableSnapshot(account.ID, provider, now, classifyBalanceError(validationErr))
	}
	return snapshot
}

func (c *BalanceCache) successSnapshot(accountID int64, provider string, result ProviderBalance, now time.Time) (BalanceSnapshot, error) {
	amount := strings.TrimSpace(result.Amount)
	value, ok := parseAmount(amount)
	if !ok || value.Sign() < 0 {
		return unavailableSnapshot(accountID, provider, now, BalanceErrorInvalidValue), errInvalidBalance
	}
	return BalanceSnapshot{
		AccountID: accountID, Provider: provider, Status: BalanceStatusFresh,
		Amount: amount, Currency: strings.TrimSpace(result.Currency), Low: c.isLow(value),
		CheckedAt: now, AttemptedAt: now, ExpiresAt: now.Add(c.ttl), StaleUntil: now.Add(c.ttl + c.staleFor),
	}, nil
}

func (c *BalanceCache) staleSnapshot(previous BalanceSnapshot, now time.Time, err error) (BalanceSnapshot, bool) {
	if previous.Amount == "" || previous.CheckedAt.IsZero() || c.staleFor <= 0 || !now.Before(previous.StaleUntil) {
		return BalanceSnapshot{}, false
	}
	previous.Status = BalanceStatusStale
	previous.AttemptedAt = now
	previous.ErrorCode = classifyBalanceError(err)
	return previous, true
}

func (c *BalanceCache) isLow(value *big.Rat) bool {
	return c.threshold != nil && value.Cmp(c.threshold) < 0
}

func unavailableSnapshot(accountID int64, provider string, now time.Time, code BalanceErrorCode) BalanceSnapshot {
	return BalanceSnapshot{AccountID: accountID, Provider: provider, Status: BalanceStatusUnavailable, AttemptedAt: now, ErrorCode: code}
}

type balanceCacheIdentity interface{ CacheIdentity() string }

func balanceCacheKey(provider string, account BalanceAccount, adapter BalanceAdapter) string {
	identity := ""
	if a, ok := adapter.(balanceCacheIdentity); ok {
		identity = a.CacheIdentity()
	}
	// Imported credentials commonly contain the copied Authorization prefix.
	// The wire request strips it, so the cache identity must do the same or one
	// credential would create two independent snapshots and refreshes.
	secretHash := sha256.Sum256([]byte(normalizeBearerKey(account.UpstreamKey)))
	return provider + "\x00" + strconv.FormatInt(account.ID, 10) + "\x00" + account.BaseURL + "\x00" + identity + "\x00" + string(secretHash[:])
}

func parseAmount(value string) (*big.Rat, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(value)
	return r, ok
}

func errorCode(err error) BalanceErrorCode {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if errors.Is(err, context.DeadlineExceeded) {
			return BalanceErrorTimeout
		}
		return BalanceErrorCanceled
	}
	return classifyBalanceError(err)
}

func classifyBalanceError(err error) BalanceErrorCode {
	if errors.Is(err, context.DeadlineExceeded) {
		return BalanceErrorTimeout
	}
	if errors.Is(err, context.Canceled) {
		return BalanceErrorCanceled
	}
	var e *balanceHTTPError
	if errors.As(err, &e) {
		switch {
		case e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden:
			return BalanceErrorAuth
		case e.StatusCode == http.StatusTooManyRequests:
			return BalanceErrorRateLimited
		}
	}
	if errors.Is(err, errInvalidBalance) {
		return BalanceErrorInvalidValue
	}
	if errors.Is(err, errNoBalanceEndpoint) {
		return BalanceErrorNoEndpoint
	}
	if errors.Is(err, errMissingKey) {
		return BalanceErrorAuth
	}
	return BalanceErrorUpstream
}

// ClassifyBalanceError exposes the bounded error vocabulary used by the
// management surface without exposing upstream response bodies or credentials.
func ClassifyBalanceError(err error) BalanceErrorCode { return classifyBalanceError(err) }

var errInvalidBalance = errors.New("invalid provider balance")
var errMissingKey = errors.New("provider balance authentication key is missing")

type balanceHTTPError struct{ StatusCode int }

func (e *balanceHTTPError) Error() string { return "provider balance request failed" }

// HTTPJSONAdapter handles the common provider shape where a JSON endpoint returns a
// balance at a dotted path. Provider-specific adapters remain preferable when the
// endpoint requires signing or pagination.
type HTTPJSONAdapter struct {
	NameValue       string
	Endpoint        string
	Method          string
	Auth            HTTPAuthStyle
	BalancePath     string
	CurrencyPath    string
	Client          *http.Client
	Timeout         time.Duration
	MaxResponseSize int64
}

type HTTPAuthStyle string

const (
	HTTPAuthNone   HTTPAuthStyle = "none"
	HTTPAuthBearer HTTPAuthStyle = "bearer"
	HTTPAuthAPIKey HTTPAuthStyle = "api_key"
)

func (a HTTPJSONAdapter) Provider() string { return strings.TrimSpace(a.NameValue) }

func (a HTTPJSONAdapter) CacheIdentity() string {
	return strings.Join([]string{strings.TrimSpace(a.Endpoint), strings.ToUpper(strings.TrimSpace(a.Method)), string(a.Auth), strings.TrimSpace(a.BalancePath), strings.TrimSpace(a.CurrencyPath)}, "\x00")
}

func (a HTTPJSONAdapter) EndpointConfigured() bool { return strings.TrimSpace(a.Endpoint) != "" }

func (a HTTPJSONAdapter) Validate() error {
	if a.Provider() == "" {
		return errors.New("provider balance name must not be empty")
	}
	if !a.EndpointConfigured() {
		return nil
	}
	auth := a.Auth
	if auth == "" {
		auth = HTTPAuthNone
	}
	if auth != HTTPAuthNone && auth != HTTPAuthBearer && auth != HTTPAuthAPIKey {
		return fmt.Errorf("provider balance auth %q is unsupported", auth)
	}
	method := strings.ToUpper(strings.TrimSpace(a.Method))
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodPost && method != http.MethodHead {
		return fmt.Errorf("provider balance method %q is unsupported", method)
	}
	if method == http.MethodHead {
		return errors.New("provider balance method HEAD cannot return a JSON balance")
	}
	if a.Timeout < 0 {
		return errors.New("provider balance timeout must be non-negative")
	}
	if a.MaxResponseSize < 0 {
		return errors.New("provider balance max response size must be non-negative")
	}
	return nil
}

func (a HTTPJSONAdapter) Fetch(ctx context.Context, account BalanceAccount) (ProviderBalance, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := a.Validate(); err != nil {
		return ProviderBalance{}, err
	}
	value, err := a.fetchJSON(ctx, account)
	if err != nil {
		return ProviderBalance{}, err
	}
	amount, ok := valueAtPath(value, a.BalancePath)
	if !ok {
		return ProviderBalance{}, errInvalidBalance
	}
	amountString, ok := scalarString(amount)
	if !ok {
		return ProviderBalance{}, errInvalidBalance
	}
	currency := ""
	if a.CurrencyPath != "" {
		if value, exists := valueAtPath(value, a.CurrencyPath); exists {
			currency, _ = scalarString(value)
		}
	}
	return ProviderBalance{Amount: amountString, Currency: currency}, nil
}

// fetchJSON performs the authenticated JSON request without imposing a field
// mapping. Automatic balance discovery uses it to try the small set of provider
// conventions supported by CC Switch while the explicit adapter keeps its dotted
// path behavior for existing records.
func (a HTTPJSONAdapter) fetchJSON(ctx context.Context, account BalanceAccount) (any, error) {
	endpoint, err := resolveEndpoint(a.Endpoint, account.BaseURL)
	if err != nil {
		return nil, err
	}
	key := normalizeBearerKey(account.UpstreamKey)
	auth := a.Auth
	if auth == "" {
		auth = HTTPAuthNone
	}
	if auth != HTTPAuthNone && key == "" {
		return nil, errMissingKey
	}
	method := strings.ToUpper(strings.TrimSpace(a.Method))
	if method == "" {
		method = http.MethodGet
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, method, endpoint, nil)
	if err != nil {
		return nil, err
	}
	switch auth {
	case HTTPAuthBearer:
		req.Header.Set("Authorization", "Bearer "+key)
	case HTTPAuthAPIKey:
		req.Header.Set("X-API-Key", key)
	}
	client := a.Client
	if client == nil {
		client = newDirectBalanceClient(timeout)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		// net/http clients should never return (nil, nil), but custom transports
		// and test doubles can violate that contract. Keep balance refreshes
		// bounded instead of dereferencing a nil response below.
		return nil, errors.New("nil provider balance response")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return nil, &balanceHTTPError{StatusCode: resp.StatusCode}
	}
	limit := a.MaxResponseSize
	if limit <= 0 {
		limit = 1 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errBalanceResponseTooLarge
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errBalanceTrailingJSON
		}
		return nil, err
	}
	return value, nil
}

// normalizeBearerKey accepts the form users most often paste from an
// Authorization header while keeping only the credential value used by the
// adapter. Repeated prefixes are collapsed so a later header construction
// cannot produce "Bearer Bearer ...".
func normalizeBearerKey(value string) string {
	value = strings.TrimSpace(value)
	for value != "" {
		fields := strings.Fields(value)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "bearer") {
			break
		}
		value = strings.TrimSpace(value[len(fields[0]):])
	}
	return value
}

type autoBalanceCandidate struct {
	path    string
	extract func(any) (ProviderBalance, bool)
}

// FetchAutoBalance follows the same provider-first strategy as CC Switch. Known
// vendors use their documented balance endpoint; relay-style deployments then get
// a bounded fallback over common /user/self and /balance shapes. It never treats a
// missing field as a zero balance and never returns response text or credentials.
func FetchAutoBalance(ctx context.Context, account BalanceAccount, client *http.Client) (ProviderBalance, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	base := strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
	if base == "" {
		return ProviderBalance{}, errInvalidBalance
	}
	parsed, err := url.Parse(base)
	if err != nil || !isHTTPURL(parsed) {
		return ProviderBalance{}, errInvalidBalance
	}
	requestCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	candidates := autoBalanceCandidates(strings.ToLower(parsed.Hostname()))
	var lastErr error
	authFailure := false
	for _, candidate := range candidates {
		adapter := HTTPJSONAdapter{
			NameValue:       "auto",
			Endpoint:        base + candidate.path,
			Method:          http.MethodGet,
			Auth:            HTTPAuthBearer,
			Client:          client,
			Timeout:         3 * time.Second,
			MaxResponseSize: 1 << 20,
		}
		value, fetchErr := adapter.fetchJSON(requestCtx, account)
		if fetchErr != nil {
			if requestCtx.Err() != nil {
				return ProviderBalance{}, requestCtx.Err()
			}
			var httpErr *balanceHTTPError
			if errors.As(fetchErr, &httpErr) {
				switch httpErr.StatusCode {
				case http.StatusUnauthorized, http.StatusForbidden:
					authFailure = true
					lastErr = fetchErr
					continue
				case http.StatusNotFound, http.StatusMethodNotAllowed:
					lastErr = fetchErr
					continue
				case http.StatusTooManyRequests:
					return ProviderBalance{}, fetchErr
				}
			}
			lastErr = fetchErr
			continue
		}
		if result, ok := candidate.extract(value); ok {
			return result, nil
		}
		lastErr = errInvalidBalance
	}
	if authFailure {
		return ProviderBalance{}, &balanceHTTPError{StatusCode: http.StatusUnauthorized}
	}
	var httpErr *balanceHTTPError
	if errors.As(lastErr, &httpErr) && (httpErr.StatusCode == http.StatusNotFound || httpErr.StatusCode == http.StatusMethodNotAllowed) {
		return ProviderBalance{}, errNoBalanceEndpoint
	}
	if lastErr == nil {
		lastErr = errNoBalanceEndpoint
	}
	return ProviderBalance{}, lastErr
}

func autoBalanceCandidates(host string) []autoBalanceCandidate {
	known := make([]autoBalanceCandidate, 0, 2)
	switch {
	case strings.Contains(host, "api.deepseek.com"):
		known = append(known, autoBalanceCandidate{path: "/user/balance", extract: extractDeepSeekBalance})
	case strings.Contains(host, "api.stepfun.ai") || strings.Contains(host, "api.stepfun.com"):
		known = append(known, autoBalanceCandidate{path: "/v1/accounts", extract: extractStepFunBalance})
	case strings.Contains(host, "api.siliconflow.cn"):
		known = append(known, autoBalanceCandidate{path: "/v1/user/info", extract: extractSiliconFlowBalance("CNY")})
	case strings.Contains(host, "api.siliconflow.com"):
		known = append(known, autoBalanceCandidate{path: "/v1/user/info", extract: extractSiliconFlowBalance("USD")})
	case strings.Contains(host, "openrouter.ai"):
		known = append(known, autoBalanceCandidate{path: "/api/v1/credits", extract: extractOpenRouterBalance})
	case strings.Contains(host, "api.novita.ai"):
		known = append(known, autoBalanceCandidate{path: "/v3/user/balance", extract: extractNovitaBalance})
	}
	known = append(known,
		// CC Switch usage-script convention used by many OpenAI-compatible
		// relays (including you.loveme.space): GET /v1/usage with the
		// remaining/quota.remaining/balance value at the response root.
		autoBalanceCandidate{path: "/v1/usage", extract: extractGenericBalance},
		autoBalanceCandidate{path: "/api/user/self", extract: extractGenericBalance},
		autoBalanceCandidate{path: "/api/user/balance", extract: extractGenericBalance},
		autoBalanceCandidate{path: "/api/balance", extract: extractGenericBalance},
		autoBalanceCandidate{path: "/v1/user/balance", extract: extractGenericBalance},
		autoBalanceCandidate{path: "/user/balance", extract: extractGenericBalance},
		autoBalanceCandidate{path: "/api/dashboard/billing/credit_grants", extract: extractGenericBalance},
	)
	return known
}

func extractDeepSeekBalance(value any) (ProviderBalance, bool) {
	root, ok := value.(map[string]any)
	if !ok {
		return ProviderBalance{}, false
	}
	infos, ok := root["balance_infos"].([]any)
	if !ok || len(infos) == 0 {
		return ProviderBalance{}, false
	}
	for _, raw := range infos {
		info, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if amount, ok := scalarString(info["total_balance"]); ok {
			currency, _ := scalarString(info["currency"])
			return ProviderBalance{Amount: amount, Currency: currency}, true
		}
	}
	return ProviderBalance{}, false
}

func extractStepFunBalance(value any) (ProviderBalance, bool) {
	return extractNamedBalance(value, "balance", "CNY")
}

func extractSiliconFlowBalance(currency string) func(any) (ProviderBalance, bool) {
	return func(value any) (ProviderBalance, bool) {
		root, ok := value.(map[string]any)
		if !ok {
			return ProviderBalance{}, false
		}
		data, ok := root["data"]
		if !ok {
			return ProviderBalance{}, false
		}
		return extractNamedBalance(data, "totalBalance", currency)
	}
}

func extractOpenRouterBalance(value any) (ProviderBalance, bool) {
	root, ok := value.(map[string]any)
	if !ok {
		return ProviderBalance{}, false
	}
	data := any(root)
	if nested, exists := root["data"]; exists {
		data = nested
	}
	obj, ok := data.(map[string]any)
	if !ok {
		return ProviderBalance{}, false
	}
	total, totalOK := scalarRat(obj["total_credits"])
	usage, usageOK := scalarRat(obj["total_usage"])
	if !totalOK || !usageOK {
		return ProviderBalance{}, false
	}
	remaining := new(big.Rat).Sub(total, usage)
	if remaining.Sign() < 0 {
		remaining.SetInt64(0)
	}
	return ProviderBalance{Amount: ratString(remaining), Currency: "USD"}, true
}

func extractNovitaBalance(value any) (ProviderBalance, bool) {
	amount, ok := scalarRatFrom(value, "availableBalance")
	if !ok {
		return ProviderBalance{}, false
	}
	amount.Quo(amount, big.NewRat(10000, 1))
	return ProviderBalance{Amount: ratString(amount), Currency: "USD"}, true
}

func extractNamedBalance(value any, name, defaultCurrency string) (ProviderBalance, bool) {
	amount, ok := scalarStringFrom(value, name)
	if !ok {
		return ProviderBalance{}, false
	}
	currency, _ := scalarStringFrom(value, "currency")
	if currency == "" {
		currency = defaultCurrency
	}
	return ProviderBalance{Amount: amount, Currency: currency}, true
}

func extractGenericBalance(value any) (ProviderBalance, bool) {
	if obj, ok := value.(map[string]any); ok {
		for _, name := range []string{"balance", "remaining", "available_balance", "availableBalance", "total_balance", "totalBalance", "credits", "credit", "quota"} {
			if amount, ok := scalarString(obj[name]); ok {
				if rat, valid := new(big.Rat).SetString(amount); valid && rat.Sign() >= 0 {
					currency, _ := scalarString(obj["currency"])
					if currency == "" {
						// CC Switch usage scripts commonly call this field `unit`.
						currency, _ = scalarString(obj["unit"])
					}
					if currency == "" {
						currency = "credits"
					}
					return ProviderBalance{Amount: amount, Currency: currency}, true
				}
			}
		}
		for _, name := range []string{"data", "result", "user", "account"} {
			if nested, exists := obj[name]; exists {
				if result, ok := extractGenericBalance(nested); ok {
					return result, true
				}
			}
		}
	}
	if items, ok := value.([]any); ok {
		for _, item := range items {
			if result, found := extractGenericBalance(item); found {
				return result, true
			}
		}
	}
	return ProviderBalance{}, false
}

func scalarStringFrom(value any, name string) (string, bool) {
	if obj, ok := value.(map[string]any); ok {
		return scalarString(obj[name])
	}
	return "", false
}

func scalarRat(value any) (*big.Rat, bool) {
	text, ok := scalarString(value)
	if !ok {
		return nil, false
	}
	return new(big.Rat).SetString(text)
}

func scalarRatFrom(value any, name string) (*big.Rat, bool) {
	if obj, ok := value.(map[string]any); ok {
		return scalarRat(obj[name])
	}
	return nil, false
}

func ratString(value *big.Rat) string {
	if value == nil {
		return ""
	}
	text := value.FloatString(12)
	text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	if text == "" || text == "-0" {
		return "0"
	}
	return text
}

var (
	errBalanceResponseTooLarge = errors.New("provider balance response too large")
	errBalanceTrailingJSON     = errors.New("provider balance response has trailing JSON")
	errNoBalanceEndpoint       = errors.New("no automatic balance endpoint matched")
)

// newDirectBalanceClient prevents a balance probe from silently inheriting
// HTTP_PROXY/HTTPS_PROXY. Provider balance checks must follow the same explicit
// transport policy as the gateway's upstream clients; callers that need a proxy
// can inject a configured Client explicitly.
func newDirectBalanceClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	}
	transport.Proxy = nil
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		// A balance request can contain a bearer/API key. Treat a redirect as a
		// failed balance read instead of allowing net/http to replay that secret
		// to a location the operator did not configure.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func resolveEndpoint(endpoint, base string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", err
	}
	if u.IsAbs() {
		if !isHTTPURL(u) {
			return "", errors.New("provider balance endpoint must use http or https")
		}
		return u.String(), nil
	}
	if strings.TrimSpace(base) == "" {
		return "", errors.New("provider balance endpoint requires an absolute URL or account base URL")
	}
	b, err := url.Parse(strings.TrimSpace(base))
	if err != nil || !b.IsAbs() || !isHTTPURL(b) {
		return "", errors.New("invalid account base URL")
	}
	resolved := b.ResolveReference(u)
	if !isHTTPURL(resolved) {
		return "", errors.New("provider balance endpoint must use http or https")
	}
	return resolved.String(), nil
}

func isHTTPURL(u *url.URL) bool {
	if u == nil || u.Host == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return true
	default:
		return false
	}
}

func valueAtPath(value any, path string) (any, bool) {
	if strings.TrimSpace(path) == "" {
		return value, true
	}
	for _, part := range strings.Split(path, ".") {
		obj, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		value, ok = obj[part]
		if !ok {
			return nil, false
		}
	}
	return value, true
}

func scalarString(value any) (string, bool) {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v), strings.TrimSpace(v) != ""
	case json.Number:
		return v.String(), v.String() != ""
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	default:
		return "", false
	}
}

var _ BalanceAdapter = HTTPJSONAdapter{}
