package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestP99LatencyUsesDedicatedHistogram(t *testing.T) {
	m := &metrics{latencySamples: map[int64]int64{1: 2, 5: 1}}
	require.Equal(t, int64(10), p99Latency(m))
}

func TestChatRequestBodyOmitsStream(t *testing.T) {
	req := newLoadtestRequest("http://example.test", "gk-test", "chat")
	var body map[string]any
	require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
	_, ok := body["stream"]
	require.False(t, ok)
	require.Equal(t, "Bearer gk-test", req.Header.Get("Authorization"))
}

func TestStreamRequestBodyEnablesStream(t *testing.T) {
	req := newLoadtestRequest("http://example.test", "gk-test", "stream")
	var body map[string]any
	require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
	require.Equal(t, true, body["stream"])
}
