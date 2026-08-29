// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package aiclient

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBoundedResponseBodyRejectsChunkedBodyOverLimit(t *testing.T) {
	transport := &responseLimitTransport{
		base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("123456")),
				Header:     make(http.Header),
			}, nil
		}),
		limit: 5,
	}
	resp, err := transport.RoundTrip(httptestRequest())
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.ErrorIs(t, err, ErrResponseTooLarge)
	require.Equal(t, "12345", string(body))
	require.NoError(t, resp.Body.Close())
}

func TestBoundedResponseBodyAllowsExactLimit(t *testing.T) {
	transport := &responseLimitTransport{
		base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("12345")),
				Header:     make(http.Header),
			}, nil
		}),
		limit: 5,
	}
	resp, err := transport.RoundTrip(httptestRequest())
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "12345", string(body))
	require.NoError(t, resp.Body.Close())
}

func TestResponseLimitTransportRejectsKnownOversizeBeforeRead(t *testing.T) {
	var reads int
	transport := &responseLimitTransport{
		base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			body := &countingReadCloser{ReadCloser: io.NopCloser(strings.NewReader("123456")), reads: &reads}
			return &http.Response{
				StatusCode:    http.StatusOK,
				Body:          body,
				ContentLength: 6,
				Header:        make(http.Header),
			}, nil
		}),
		limit: 5,
	}
	resp, err := transport.RoundTrip(httptestRequest())
	require.ErrorIs(t, err, ErrResponseTooLarge)
	require.Nil(t, resp)
	require.Zero(t, reads, "known Content-Length must be rejected before reading")
}

func TestBoundedHTTPClientUsesDefaultLimit(t *testing.T) {
	hc := boundedHTTPClient(&http.Client{}, 0)
	require.NotNil(t, hc)
	rt, ok := hc.Transport.(*responseLimitTransport)
	require.True(t, ok)
	require.Equal(t, defaultMaxResponseSize, rt.limit)
}

func TestBoundedHTTPClientFollowsSourceTransportReplacement(t *testing.T) {
	var first, second int
	hc := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		first++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("one")), Header: make(http.Header)}, nil
	})}
	bounded := boundedHTTPClient(hc, 8)
	req := httptestRequest()
	resp, err := bounded.Transport.RoundTrip(req)
	require.NoError(t, err)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, 1, first)
	hc.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		second++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("two")), Header: make(http.Header)}, nil
	})
	resp, err = bounded.Transport.RoundTrip(req)
	require.NoError(t, err)
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, 1, second)
}

type countingReadCloser struct {
	io.ReadCloser
	reads *int
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	(*c.reads)++
	return c.ReadCloser.Read(p)
}

func httptestRequest() *http.Request {
	req, _ := http.NewRequest(http.MethodGet, "http://example.test", nil)
	return req
}
