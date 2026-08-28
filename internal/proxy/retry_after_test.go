package proxy

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestRetryAfterDeadline(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	got := retryAfterDeadline(now, http.Header{"Retry-After": []string{"5"}}, nil)
	if got == nil || !got.Equal(now.Add(5*time.Second)) {
		t.Fatalf("seconds header: got %v", got)
	}

	date := now.Add(20 * time.Second).Format(http.TimeFormat)
	got = retryAfterDeadline(now, http.Header{"Retry-After": []string{date}}, nil)
	if got == nil || !got.Equal(now.Add(20*time.Second)) {
		t.Fatalf("date header: got %v", got)
	}

	got = retryAfterDeadline(now, http.Header{"Retry-After-Ms": []string{"250"}}, nil)
	if got == nil || !got.Equal(now.Add(time.Second)) {
		t.Fatalf("minimum delay: got %v", got)
	}

	if got := retryAfterDeadline(now, http.Header{"Retry-After": []string{"garbage"}}, nil); got != nil {
		t.Fatalf("invalid header: got %v", got)
	}
}

func TestRetryAfterDeadlineFromResponseError(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	err := &responseHeadersError{header: http.Header{"Retry-After": []string{"7"}}, err: errors.New("rate limited")}
	got := retryAfterDeadline(now, nil, err)
	if got == nil || !got.Equal(now.Add(7*time.Second)) {
		t.Fatalf("wrapped header: got %v", got)
	}
}
