// SPDX-License-Identifier: AGPL-3.0-or-later
package httpx

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type switchTestRoundTripper struct {
	name  string
	count atomic.Int64
}

type blockingRoundTripper struct {
	started chan struct{}
	release chan struct{}
	closed  atomic.Int64
}

func (r *blockingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	close(r.started)
	<-r.release
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("old"))}, nil
}

func (r *blockingRoundTripper) CloseIdleConnections() { r.closed.Add(1) }

func (r *switchTestRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	r.count.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(r.name)),
	}, nil
}

func TestSwitchableTransportAtomicConcurrentSwap(t *testing.T) {
	first := &switchTestRoundTripper{name: "first"}
	second := &switchTestRoundTripper{name: "second"}
	sw, err := NewSwitchableTransport(first)
	require.NoError(t, err)

	const workers = 32
	const rounds = 250
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < rounds; j++ {
				req, reqErr := http.NewRequest(http.MethodGet, "http://fixture.invalid", nil)
				if reqErr != nil {
					t.Errorf("new request: %v", reqErr)
					return
				}
				resp, rtErr := sw.RoundTrip(req)
				if rtErr != nil {
					t.Errorf("round trip: %v", rtErr)
					return
				}
				body, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					t.Errorf("read response: %v", readErr)
					return
				}
				if got := string(body); got != "first" && got != "second" {
					t.Errorf("mixed route result: %q", got)
					return
				}
			}
		}()
	}
	close(start)
	for i := 0; i < workers*2; i++ {
		if i%2 == 0 {
			sw.Swap(second)
		} else {
			sw.Swap(first)
		}
	}
	wg.Wait()
	require.Equal(t, int64(workers*rounds), first.count.Load()+second.count.Load())
}

func TestSwitchableTransportRejectsNilAndKeepsCurrent(t *testing.T) {
	first := &switchTestRoundTripper{name: "first"}
	sw, err := NewSwitchableTransport(first)
	require.NoError(t, err)
	require.Nil(t, sw.Swap(nil))
	require.Same(t, first, sw.Current())
	_, err = NewSwitchableTransport(nil)
	require.Error(t, err)
	var typedNil *switchTestRoundTripper
	_, err = NewSwitchableTransport(typedNil)
	require.Error(t, err, "typed-nil round tripper must be rejected")
	_, _, _, err = NewTransportPair(typedNil, first)
	require.Error(t, err, "typed-nil pair route must be rejected")
	require.Nil(t, sw.Swap(typedNil))
	require.Same(t, first, sw.Current())
}

func TestSwitchableTransportSwapDoesNotInterruptInFlightRequest(t *testing.T) {
	old := &blockingRoundTripper{started: make(chan struct{}), release: make(chan struct{})}
	newRoute := &switchTestRoundTripper{name: "new"}
	sw, err := NewSwitchableTransport(old)
	require.NoError(t, err)
	result := make(chan string, 1)
	go func() {
		resp, roundTripErr := sw.RoundTrip(nil)
		if roundTripErr != nil {
			result <- roundTripErr.Error()
			return
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			result <- readErr.Error()
			return
		}
		result <- string(body)
	}()
	<-old.started
	require.Same(t, old, sw.Swap(newRoute))
	CloseIdle(old)
	close(old.release)
	require.Equal(t, "old", <-result)
	require.Equal(t, int64(1), old.closed.Load())
	require.Equal(t, "new", func() string {
		resp, e := sw.RoundTrip(nil)
		require.NoError(t, e)
		defer resp.Body.Close()
		b, e := io.ReadAll(resp.Body)
		require.NoError(t, e)
		return string(b)
	}())
}

func TestTransportPairSnapshotNeverPublishesHalfSwitch(t *testing.T) {
	oldGateway := &switchTestRoundTripper{name: "old-gateway"}
	oldCodex := &switchTestRoundTripper{name: "old-codex"}
	newGateway := &switchTestRoundTripper{name: "new-gateway"}
	newCodex := &switchTestRoundTripper{name: "new-codex"}
	pair, gateway, codex, err := NewTransportPair(oldGateway, oldCodex)
	require.NoError(t, err)

	const rounds = 1000
	start := make(chan struct{})
	errs := make(chan string, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < rounds; i++ {
			if i%2 == 0 {
				pair.Swap(newGateway, newCodex)
			} else {
				pair.Swap(oldGateway, oldCodex)
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < rounds*4; i++ {
			first, second := pair.Snapshot()
			switch {
			case first == oldGateway:
				if second != oldCodex {
					errs <- "old gateway paired with a non-old Codex route"
					return
				}
			case first == newGateway:
				if second != newCodex {
					errs <- "new gateway paired with a non-new Codex route"
					return
				}
			default:
				errs <- "unexpected gateway route"
				return
			}
			// The interface wrappers must point at the same published generation.
			if gateway.Current() == nil || codex.Current() == nil {
				errs <- "pair wrapper exposed a nil route"
				return
			}
		}
	}()
	close(start)
	wg.Wait()
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
}
