package aiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
)

func testTemplate(baseURL string) *domain.Template {
	return &domain.Template{ID: 1, BaseURL: baseURL}
}

func TestChatCompletionStreamRawHeadersAndPath(t *testing.T) {
	var gotAuth, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: x\n\n"))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	resp, err := f.ChatCompletionStreamRaw(context.Background(), testTemplate(srv.URL+"/v1"), "sk-test", []byte(`{"stream":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "Bearer sk-test", gotAuth)
	require.Equal(t, "application/json", gotCT)
}

func TestAnthMessageStreamRawUsesXAPIKeyAndV1Path(t *testing.T) {
	var gotKey, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotCT = r.Header.Get("Content-Type")
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {}\n\n"))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	resp, err := f.AnthMessageStreamRaw(context.Background(), testTemplate(srv.URL), "sk-anth", []byte(`{"stream":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "sk-anth", gotKey)
	require.Equal(t, "application/json", gotCT)
}

func TestResponseStreamRawPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: x\n\n"))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	resp, err := f.ResponseStreamRaw(context.Background(), testTemplate(srv.URL+"/v1"), "sk-test", []byte(`{"stream":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestStreamRawPreservesRequestBody(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: x\n\n"))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	resp, err := f.ChatCompletionStreamRaw(context.Background(), testTemplate(srv.URL+"/v1"), "sk-test", []byte(`{"model":"gpt-4o","stream":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "gpt-4o", gotBody["model"])
	require.Equal(t, true, gotBody["stream"])
}

func TestStreamRawNon200ResponseReturnsResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad"}}`))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	resp, err := f.ChatCompletionStreamRaw(context.Background(), testTemplate(srv.URL+"/v1"), "sk-test", []byte(`{"stream":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestStreamRawBaseURLWithTrailingSlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: x\n\n"))
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	resp, err := f.ChatCompletionStreamRaw(context.Background(), testTemplate(srv.URL+"/v1/"), "sk-test", []byte(`{"stream":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestStreamRawContextTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	f := NewFactory(srv.Client(), Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := f.ChatCompletionStreamRaw(ctx, testTemplate(srv.URL+"/v1"), "sk-test", []byte(`{"stream":true}`))
	require.Error(t, err)
}
