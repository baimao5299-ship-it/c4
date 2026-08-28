// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"
)

const (
	startupRetryInitial = 250 * time.Millisecond
	startupRetryMax     = 2 * time.Second
)

// retryStartup absorbs a dependency's short restart window without turning a
// transient outage into a container restart loop. Permanent configuration or
// authentication errors still return immediately through retryable.
func retryStartup(ctx context.Context, op func() error, retryable func(error) bool, onRetry func(error, time.Duration)) error {
	return retryStartupWithBackoff(ctx, startupRetryInitial, startupRetryMax, op, retryable, onRetry)
}

func retryStartupWithBackoff(ctx context.Context, initial, max time.Duration, op func() error, retryable func(error) bool, onRetry func(error, time.Duration)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if op == nil {
		return errors.New("startup retry operation is not configured")
	}
	if retryable == nil {
		retryable = func(error) bool { return true }
	}
	if initial <= 0 {
		initial = startupRetryInitial
	}
	if max < initial {
		max = initial
	}
	delay := initial
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return errors.Join(lastErr, err)
			}
			return err
		}
		err := op()
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable(err) {
			return err
		}
		timer := time.NewTimer(delay)
		if onRetry != nil {
			onRetry(err, delay)
		}
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
		if delay < max {
			delay *= 2
			if delay > max {
				delay = max
			}
		}
	}
}

// retryableStartupError intentionally matches connection/readiness failures
// only. Bad credentials, invalid SQL, and schema errors should fail fast.
func retryableStartupError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection refused",
		"connection reset",
		"connection closed",
		"server closed the connection",
		"database system is starting up",
		"temporarily unavailable",
		"no route to host",
		"i/o timeout",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
