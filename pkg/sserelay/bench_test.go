// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sserelay

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 100 个 chunk 的典型高频流（对应压测 fakeupstream 100 × 20ms 的流形态）
func sseStream100() string {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString(`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"x"},"index":0}]}`)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func BenchmarkRelay100Chunks(b *testing.B) {
	src := sseStream100()
	var sink bytes.Buffer
	cfg := Config{FlushBytes: 4096, FlushInterval: time.Millisecond}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink.Reset()
		if err := Relay(context.Background(), &benchWriter{&sink}, strings.NewReader(src), cfg); err != nil {
			b.Fatal(err)
		}
	}
}

// benchWriter 刻意不实现 http.Flusher：无 Flush syscall，衡量纯 relay 成本。
type benchWriter struct{ *bytes.Buffer }

func (w *benchWriter) Header() http.Header { return http.Header{} }
func (w *benchWriter) WriteHeader(int)     {}
