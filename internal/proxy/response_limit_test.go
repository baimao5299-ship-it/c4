// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadUpstreamResponseRejectsOversizeChunkedBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("123456")),
		Header:     make(http.Header),
	}
	body, err := readUpstreamResponse(resp, 5)
	require.ErrorIs(t, err, errUpstreamResponseTooLarge)
	require.Nil(t, body)
	require.NoError(t, resp.Body.Close())
}

func TestReadUpstreamResponseAllowsExactLimit(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("12345")),
		Header:     make(http.Header),
	}
	body, err := readUpstreamResponse(resp, 5)
	require.NoError(t, err)
	require.Equal(t, "12345", string(body))
	require.NoError(t, resp.Body.Close())
}

func TestReadUpstreamResponseRejectsKnownOversizeWithoutReading(t *testing.T) {
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Body:          io.NopCloser(strings.NewReader("123456")),
		ContentLength: 6,
		Header:        make(http.Header),
	}
	body, err := readUpstreamResponse(resp, 5)
	require.ErrorIs(t, err, errUpstreamResponseTooLarge)
	require.Nil(t, body)
	require.NoError(t, resp.Body.Close())
}

func TestResponseSizeLimitUsesDefaultWhenUnset(t *testing.T) {
	require.Equal(t, defaultMaxResponseSize, responseSizeLimit(0))
	require.Equal(t, defaultMaxResponseSize, responseSizeLimit(-1))
	require.Equal(t, int64(5), responseSizeLimit(5))
}
