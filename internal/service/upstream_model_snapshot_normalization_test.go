// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestIsUpstreamSuccessResponseAcceptsMinimalKnownSSEOutputEvents(t *testing.T) {
	for _, body := range [][]byte{
		[]byte("event: response.output_text.delta\ndata: {\"delta\":\"hi\"}\n\n"),
		[]byte("event: response.completed\ndata: {}\n\n"),
		[]byte("event: content_block_delta\ndata: {\"delta\":{\"text\":\"hi\"}}\n\n"),
	} {
		require.Truef(t, isUpstreamSuccessResponse(body), "known output event should prove capability: %q", body)
	}
	for _, body := range [][]byte{
		[]byte("event: response.created\ndata: {}\n\n"),
		[]byte("event: response.output_text.delta\ndata: {\"error\":{\"message\":\"failed\"}}\n\n"),
		[]byte("event: response.unknown\ndata: {}\n\n"),
	} {
		require.Falsef(t, isUpstreamSuccessResponse(body), "neutral/error/unknown event must not prove capability: %q", body)
	}
}

func TestRetainedUpstreamModelFormatsNormalizesLegacyKeys(t *testing.T) {
	current := map[string][]domain.RequestFormat{
		"  claude-opus-4-6  ": {domain.FormatAnthropic},
	}
	expected := &domain.Upstream{
		ModelFormats: map[string][]domain.RequestFormat{
			"  claude-opus-4-6  ": {domain.FormatOpenAIChat},
		},
	}

	got := retainedUpstreamModelFormats(expected, []string{"claude-opus-4-6"}, current, false)
	require.Equal(t, map[string][]domain.RequestFormat{
		"claude-opus-4-6": {domain.FormatAnthropic},
	}, got, "a current probe result must win even when its legacy key has whitespace")
}

func TestRetainedUpstreamModelFormatsFallsBackToLegacyKeyOnIncompleteProbe(t *testing.T) {
	expected := &domain.Upstream{
		ModelFormats: map[string][]domain.RequestFormat{
			"  gpt-5.6  ": {domain.FormatOpenAIResponses},
		},
	}

	got := retainedUpstreamModelFormats(expected, []string{"gpt-5.6"}, nil, false)
	require.Equal(t, map[string][]domain.RequestFormat{
		"gpt-5.6": {domain.FormatOpenAIResponses},
	}, got, "an incomplete refresh must preserve the prior protocol capability")
}

func TestRetainedUpstreamModelFormatsPrefersCanonicalDuplicateKey(t *testing.T) {
	current := map[string][]domain.RequestFormat{
		"gpt-5":   {domain.FormatOpenAIResponses},
		" gpt-5 ": {domain.FormatOpenAIChat},
	}

	got := retainedUpstreamModelFormats(nil, []string{"gpt-5"}, current, true)
	require.Equal(t, map[string][]domain.RequestFormat{
		"gpt-5": {domain.FormatOpenAIResponses},
	}, got, "the canonical key is the authoritative one when old and new keys coexist")
}
