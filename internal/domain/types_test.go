package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTemplateFormatSupports(t *testing.T) {
	tpl := &Template{
		SupportedFormats: []RequestFormat{FormatOpenAIChat, FormatAnthropic},
		Models:           []string{"gpt-4o", "claude-3"},
		FormatModels:     map[RequestFormat][]string{FormatOpenAIChat: {"gpt-4o"}},
	}
	require.True(t, tpl.FormatSupports(FormatOpenAIChat, "gpt-4o"))
	require.False(t, tpl.FormatSupports(FormatOpenAIChat, "claude-3"), "配置了格式 → 仅列表内模型")
	require.True(t, tpl.FormatSupports(FormatAnthropic, "gpt-4o"), "未配置格式 → 全部模型")
	require.False(t, tpl.FormatSupports(FormatOpenAIResponses, "gpt-4o"), "格式不在 supported")
	require.True(t, tpl.Serves("gpt-4o"))
	require.False(t, tpl.Serves("nonexistent"))
	require.Equal(t, []RequestFormat{FormatOpenAIChat, FormatAnthropic}, tpl.FormatsFor())
}

func TestTemplateServes(t *testing.T) {
	tpl := &Template{
		Models:       []string{"gpt-4o"},
		FormatModels: map[RequestFormat][]string{FormatOpenAIResponses: {"o3"}},
		ModelMapping: map[string]string{"claude-sonnet": "claude-sonnet-4-5"},
	}
	require.True(t, tpl.Serves("gpt-4o"), "serves models")
	require.True(t, tpl.Serves("o3"), "serves format_models list values")
	require.True(t, tpl.Serves("claude-sonnet"), "serves mapping keys")
	require.False(t, tpl.Serves("nope"))
}

func TestRequestFormatValid(t *testing.T) {
	for _, f := range []RequestFormat{FormatOpenAIChat, FormatOpenAIResponses, FormatAnthropic} {
		require.True(t, f.Valid(), "format %s should be valid", f)
	}
	require.False(t, RequestFormat("gemini").Valid())
}
