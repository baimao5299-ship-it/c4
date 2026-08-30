// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProxyRuntimeRetrySnapshotCannotRestoreChangedRoute(t *testing.T) {
	r, _, _ := newRuntimeTest(t)
	r.probe = nil

	r.mu.RLock()
	stale := proxyRouteSnapshot{proxyURL: r.proxyURL, generation: r.generation}
	r.mu.RUnlock()

	// A foreground operator switch completes after the worker read its snapshot.
	require.NoError(t, r.Apply(context.Background(), "http://new:2"))
	require.Equal(t, "http://new:2", r.ProxyURL())

	// The delayed worker tick must be ignored instead of restoring the old route.
	require.NoError(t, r.apply(context.Background(), stale.proxyURL, &stale))
	require.Equal(t, "http://new:2", r.ProxyURL())
	got, ok := r.gateway.Current().(*runtimeTestRoundTripper)
	require.True(t, ok)
	require.Equal(t, "http-gateway", got.label)
}
