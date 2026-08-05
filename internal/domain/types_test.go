package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTemplateFormatFor(t *testing.T) {
	tpl := &Template{
		DefaultFormat: FormatOpenAIChat,
		ModelFormats:  map[string]RequestFormat{"o3": FormatOpenAIResponses},
	}
	require.Equal(t, FormatOpenAIChat, tpl.FormatFor("gpt-4o"))
	require.Equal(t, FormatOpenAIResponses, tpl.FormatFor("o3"))
}

func TestTemplateServes(t *testing.T) {
	tpl := &Template{
		Models:       []string{"gpt-4o"},
		ModelFormats: map[string]RequestFormat{"o3": FormatOpenAIResponses},
		ModelMapping: map[string]string{"claude-sonnet": "claude-sonnet-4-5"},
	}
	require.True(t, tpl.Serves("gpt-4o"), "serves models")
	require.True(t, tpl.Serves("o3"), "serves model_formats keys")
	require.True(t, tpl.Serves("claude-sonnet"), "serves mapping keys")
	require.False(t, tpl.Serves("nope"))
}

func TestRequestFormatValid(t *testing.T) {
	for _, f := range []RequestFormat{FormatOpenAIChat, FormatOpenAIResponses, FormatAnthropic} {
		require.True(t, f.Valid(), "format %s should be valid", f)
	}
	require.False(t, RequestFormat("gemini").Valid())
}
