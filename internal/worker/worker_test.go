// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeWorker struct {
	name    string
	events  *[]string
	mu      *sync.Mutex
	startFn func() error
}

func (f *fakeWorker) Name() string { return f.name }
func (f *fakeWorker) Start(context.Context) error {
	// events/mu 可为 nil（如 TestStartAllTwice 只关心双启动报错，不关心事件序列）
	if f.mu != nil {
		f.mu.Lock()
		*f.events = append(*f.events, "start:"+f.name)
		f.mu.Unlock()
	}
	if f.startFn != nil {
		return f.startFn()
	}
	return nil
}
func (f *fakeWorker) Close(context.Context) error {
	if f.mu != nil {
		f.mu.Lock()
		*f.events = append(*f.events, "close:"+f.name)
		f.mu.Unlock()
	}
	return nil
}

func TestStartAllShutdownOrder(t *testing.T) {
	var events []string
	var mu sync.Mutex
	m := New(nil)
	m.Register(&fakeWorker{name: "a", events: &events, mu: &mu})
	m.Register(&fakeWorker{name: "b", events: &events, mu: &mu})
	require.NoError(t, m.StartAll(context.Background()))
	require.Equal(t, []string{"start:a", "start:b"}, events)
	require.NoError(t, m.Shutdown(context.Background()))
	require.Equal(t, []string{"start:a", "start:b", "close:b", "close:a"}, events)
}

func TestStartAllRollback(t *testing.T) {
	var events []string
	var mu sync.Mutex
	m := New(nil)
	m.Register(&fakeWorker{name: "a", events: &events, mu: &mu})
	m.Register(&fakeWorker{name: "b", events: &events, mu: &mu, startFn: func() error { return context.DeadlineExceeded }})
	err := m.StartAll(context.Background())
	require.Error(t, err)
	require.Equal(t, []string{"start:a", "start:b", "close:a"}, events)
}

func TestStartAllTwice(t *testing.T) {
	m := New(nil)
	m.Register(&fakeWorker{name: "a"})
	require.NoError(t, m.StartAll(context.Background()))
	require.Error(t, m.StartAll(context.Background()))
}

func TestGoCatchesPanic(t *testing.T) {
	m := New(nil)
	done := make(chan struct{})
	m.Go(context.Background(), "boom", func(context.Context) {
		defer close(done)
		panic("test panic")
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine did not run")
	}
}
