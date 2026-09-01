// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestValidateRequestParameterTypes(t *testing.T) {
	tests := []struct {
		name   string
		format domain.RequestFormat
		body   string
		want   string
	}{
		{
			name:   "chat integer string",
			format: domain.FormatOpenAIChat,
			body:   `{"model":"m","messages":[],"max_tokens":"10"}`,
			want:   "max_tokens must be an integer",
		},
		{
			name:   "chat messages object",
			format: domain.FormatOpenAIChat,
			body:   `{"model":"m","messages":{},"temperature":0.2}`,
			want:   "messages must be an array",
		},
		{
			name:   "responses integer decimal",
			format: domain.FormatOpenAIResponses,
			body:   `{"model":"m","input":"hi","max_output_tokens":1.0}`,
			want:   "max_output_tokens must be an integer",
		},
		{
			name:   "responses boolean string",
			format: domain.FormatOpenAIResponses,
			body:   `{"model":"m","input":"hi","background":"false"}`,
			want:   "background must be a boolean",
		},
		{
			name:   "anthropic integer string",
			format: domain.FormatAnthropic,
			body:   `{"model":"m","messages":[],"max_tokens":"1024"}`,
			want:   "max_tokens must be an integer",
		},
		{
			name:   "anthropic messages object",
			format: domain.FormatAnthropic,
			body:   `{"model":"m","messages":{},"max_tokens":1024}`,
			want:   "messages must be an array",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRequestParameterTypes(tt.format, []byte(tt.body))
			require.EqualError(t, err, tt.want)
		})
	}
}

func TestValidateRequestParameterTypesAllowsNullAndExtensions(t *testing.T) {
	cases := []struct {
		format domain.RequestFormat
		body   string
	}{
		{domain.FormatOpenAIChat, `{"model":"m","messages":[],"max_tokens":null,"vendor_extension":{"mode":1}}`},
		{domain.FormatOpenAIResponses, `{"model":"m","input":"hi","max_output_tokens":null,"vendor_extension":true}`},
		{domain.FormatAnthropic, `{"model":"m","messages":[],"max_tokens":null,"vendor_extension":[1]}`},
		{domain.FormatAnthropic, `{"model":"m","messages":[],"max_tokens":1024,"system":"be concise"}`},
	}
	for _, tc := range cases {
		require.NoError(t, validateRequestParameterTypes(tc.format, []byte(tc.body)))
	}
}

func TestValidateRequestParameterTypesRejectsNonObjectRoot(t *testing.T) {
	for _, format := range []domain.RequestFormat{
		domain.FormatOpenAIChat,
		domain.FormatOpenAIResponses,
		domain.FormatAnthropic,
	} {
		require.ErrorIs(t, validateRequestParameterTypes(format, []byte(`[1,2,3]`)), errBodyNotObject)
	}
}
