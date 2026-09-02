package proxy

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestLogWithCtxDoesNotEstimatePreUpstreamClientAbort(t *testing.T) {
	rm := &reqMeta{billingEnabled: true, estimatedInputTokens: 40}
	ctx := context.WithValue(context.Background(), ctxKeyReqMeta{}, rm)
	l := &domain.UsageLog{
		Format: domain.FormatOpenAIChat, ErrorType: domain.ErrAbort,
		StatusCode: statusClientClosedRequest,
	}

	got := logWithCtx(ctx, l)
	require.Zero(t, got.InputTokens)
	require.Zero(t, got.OutputTokens)
	require.Zero(t, got.TotalTokens)
}

func TestLogWithCtxEstimatesAbortAfterUpstreamStarted(t *testing.T) {
	rm := &reqMeta{billingEnabled: true, estimatedInputTokens: 40}
	ctx := context.WithValue(context.Background(), ctxKeyReqMeta{}, rm)
	l := &domain.UsageLog{
		Format: domain.FormatOpenAIChat, ErrorType: domain.ErrAbort,
		StatusCode: http.StatusOK,
	}

	got := logWithCtx(ctx, l)
	require.Equal(t, int64(40), got.InputTokens)
	require.Equal(t, int64(1), got.OutputTokens)
	require.Equal(t, int64(41), got.TotalTokens)
}
