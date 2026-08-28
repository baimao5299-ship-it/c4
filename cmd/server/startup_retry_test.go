// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRetryStartupRecoversAfterTransientFailure(t *testing.T) {
	var attempts int
	var delays []time.Duration
	err := retryStartupWithBackoff(context.Background(), time.Millisecond, 2*time.Millisecond, func() error {
		attempts++
		if attempts < 3 {
			return errors.New("connection refused")
		}
		return nil
	}, retryableStartupError, func(_ error, delay time.Duration) { delays = append(delays, delay) })
	require.NoError(t, err)
	require.Equal(t, 3, attempts)
	require.Equal(t, []time.Duration{time.Millisecond, 2 * time.Millisecond}, delays)
}

func TestRetryStartupFailsFastForPermanentFailure(t *testing.T) {
	var attempts int
	err := retryStartupWithBackoff(context.Background(), time.Millisecond, 2*time.Millisecond, func() error {
		attempts++
		return errors.New("password authentication failed")
	}, retryableStartupError, nil)
	require.EqualError(t, err, "password authentication failed")
	require.Equal(t, 1, attempts)
}

func TestRetryStartupReturnsDeadlineAndLastError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	var attempts int
	err := retryStartupWithBackoff(ctx, time.Millisecond, time.Millisecond, func() error {
		attempts++
		return errors.New("database system is starting up")
	}, retryableStartupError, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Contains(t, err.Error(), "database system is starting up")
	require.GreaterOrEqual(t, attempts, 2)
}
