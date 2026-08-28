// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package httpx

import (
	"errors"
	"net/http"
	"reflect"
	"sync/atomic"
)

// SwitchableTransport is a request-safe indirection over an HTTP transport.
//
// A request takes one immutable snapshot of the current RoundTripper. Swapping
// the snapshot therefore cannot expose a partially updated proxy configuration:
// in-flight requests finish on the old transport and requests that start after
// the swap use the new one. CloseIdleConnections is intentionally performed by
// the owner after a successful swap; it never interrupts an active request.
type SwitchableTransport struct {
	current atomic.Pointer[roundTripperState]
	pair    *TransportPair
	slot    uint8
}

type roundTripperState struct {
	rt http.RoundTripper
}

// NewSwitchableTransport creates a switchable transport with an initial route.
// A nil route is rejected at construction time so a bad runtime update cannot
// turn a healthy client into a nil-transport panic.
func NewSwitchableTransport(initial http.RoundTripper) (*SwitchableTransport, error) {
	if isNilRoundTripper(initial) {
		return nil, errors.New("httpx: switchable transport requires an initial round tripper")
	}
	s := &SwitchableTransport{}
	s.current.Store(&roundTripperState{rt: initial})
	return s, nil
}

// TransportPair publishes two related routes (for example the gateway and
// Codex clients) in one atomic snapshot. Each returned transport keeps the
// normal RoundTripper interface, while each request selects from the same
// published pair generation. This prevents a proxy update from exposing a
// half-switched route between the two client families. Call Snapshot when a
// caller needs to inspect both routes as one atomic observation.
type TransportPair struct {
	current atomic.Pointer[transportPairState]
}

type transportPairState struct {
	routes [2]http.RoundTripper
}

// NewTransportPair creates gateway and secondary transports backed by one
// atomic route set.
func NewTransportPair(first, second http.RoundTripper) (*TransportPair, *SwitchableTransport, *SwitchableTransport, error) {
	if isNilRoundTripper(first) || isNilRoundTripper(second) {
		return nil, nil, nil, errors.New("httpx: transport pair requires two round trippers")
	}
	p := &TransportPair{}
	p.current.Store(&transportPairState{routes: [2]http.RoundTripper{first, second}})
	return p,
		&SwitchableTransport{pair: p, slot: 0},
		&SwitchableTransport{pair: p, slot: 1},
		nil
}

// Swap atomically publishes both routes and returns the replaced pair.
func (p *TransportPair) Swap(first, second http.RoundTripper) (oldFirst, oldSecond http.RoundTripper) {
	if p == nil || isNilRoundTripper(first) || isNilRoundTripper(second) {
		return nil, nil
	}
	old := p.current.Swap(&transportPairState{routes: [2]http.RoundTripper{first, second}})
	if old == nil {
		return nil, nil
	}
	return old.routes[0], old.routes[1]
}

// Snapshot returns both routes from one published generation. It is useful
// to owners that need to inspect or coordinate the two related transports
// without observing a half-updated pair.
func (p *TransportPair) Snapshot() (first, second http.RoundTripper) {
	if p == nil {
		return nil, nil
	}
	state := p.current.Load()
	if state == nil {
		return nil, nil
	}
	return state.routes[0], state.routes[1]
}

func (p *TransportPair) currentRoute(slot uint8) http.RoundTripper {
	if p == nil || slot > 1 {
		return nil
	}
	state := p.current.Load()
	if state == nil {
		return nil
	}
	return state.routes[slot]
}

// RoundTrip delegates to the route selected when the request starts.
func (s *SwitchableTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if s == nil {
		return nil, errors.New("httpx: nil switchable transport")
	}
	if s.pair != nil {
		rt := s.pair.currentRoute(s.slot)
		if rt == nil {
			return nil, errors.New("httpx: switchable transport has no route")
		}
		return rt.RoundTrip(req)
	}
	state := s.current.Load()
	if state == nil || state.rt == nil {
		return nil, errors.New("httpx: switchable transport has no route")
	}
	return state.rt.RoundTrip(req)
}

// Current returns the currently selected route. The returned value is stable
// for the lifetime of any request that already loaded it.
func (s *SwitchableTransport) Current() http.RoundTripper {
	if s == nil {
		return nil
	}
	if s.pair != nil {
		return s.pair.currentRoute(s.slot)
	}
	state := s.current.Load()
	if state == nil {
		return nil
	}
	return state.rt
}

// Swap atomically selects next and returns the route that was replaced. The
// caller owns closing the old route after all related transports have swapped.
func (s *SwitchableTransport) Swap(next http.RoundTripper) http.RoundTripper {
	if s == nil || isNilRoundTripper(next) || s.pair != nil {
		return nil
	}
	old := s.current.Swap(&roundTripperState{rt: next})
	if old == nil {
		return nil
	}
	return old.rt
}

// CloseIdleConnections forwards the standard optional transport cleanup hook.
func (s *SwitchableTransport) CloseIdleConnections() {
	if rt := s.Current(); rt != nil {
		if closer, ok := rt.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
}

// CloseIdle closes idle connections on a replaced route without touching the
// current route. It is safe to call with nil and is useful after Swap.
func CloseIdle(rt http.RoundTripper) {
	if isNilRoundTripper(rt) {
		return
	}
	if closer, ok := rt.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// IsNilRoundTripper reports both a nil interface and an interface containing a
// typed-nil pointer. Runtime builders use this before publishing a candidate so
// a failed construction cannot advance the selected-route diagnostics.
func IsNilRoundTripper(rt http.RoundTripper) bool { return isNilRoundTripper(rt) }

// isNilRoundTripper catches both a nil interface and an interface containing a
// typed-nil pointer. The latter otherwise passes an ordinary == nil check and
// can panic only when RoundTrip is first called.
func isNilRoundTripper(rt http.RoundTripper) bool {
	if rt == nil {
		return true
	}
	v := reflect.ValueOf(rt)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
