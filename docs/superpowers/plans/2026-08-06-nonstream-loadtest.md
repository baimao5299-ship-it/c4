# Non-Stream Loadtest Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend `tools/loadtest` with a `-mode stream|chat` switch so non-streaming SDK performance can be measured with complete-response latency, then run the benchmark under a 50% CPU cap and report results.

**Architecture:** Keep one loadtest binary and one worker loop. `stream` remains the default and keeps current first-byte/SSE behavior. `chat` sends the existing request without `stream:true`, drains the complete JSON body, and records request-to-body-complete latency in a separate histogram. The benchmark harness remains outside the repository; temporary pprof builds and remote files are never committed or documented with server identity.

**Tech Stack:** Go 1.26.5, standard `flag`, `net/http`, `bufio`, `sync/atomic`, `sync.Mutex`, testify.

## Global Constraints

- `-mode` defaults to `stream`; existing stream output and behavior remain compatible.
- `chat` requests target `/v1/chat/completions` without `stream:true`.
- Chat success means HTTP 200 and the entire response body was read successfully.
- Chat HTTP errors and body-read errors increment both `total` and `errs`, close the body, and add `status:<code>` or `read:<error>` details.
- Chat reports `mode=chat`, `avg_latency_ms`, and `p99_latency_ms`; it does not report first-byte metrics.
- No gateway production code, third-party dependency, or second loadtest binary is added.
- Benchmark processes and benchmark PostgreSQL must use at most 12 of 24 CPUs: CPU 0-11, `GOMAXPROCS=12`, Docker `cpuset-cpus=0-11`.
- Do not write the benchmark server IP, hostname, SSH address, or credentials to Git, code, documentation, or committed result files.
- Run `go test ./...`, `go vet ./...`, and `golangci-lint run ./...` before benchmarking.

---

### Task 1: Add mode-aware loadtest metrics and request execution

**Files:**
- Modify: `tools/loadtest/main.go`
- Test: `tools/loadtest/main_test.go`

**Interfaces:**
- `loadtestMode` is a string flag value with `stream` and `chat` semantics.
- `doRequest` branches on the selected mode while preserving the existing stream path.
- Chat latency samples use a dedicated histogram and `p99Latency` helper so first-byte and full-response latency cannot be mixed.

- [ ] **Step 1: Write failing unit tests**

Add tests that exercise real helper behavior without starting a server:

```go
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
}

func TestStreamRequestBodyEnablesStream(t *testing.T) {
	req := newLoadtestRequest("http://example.test", "gk-test", "stream")
	var body map[string]any
	require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
	require.Equal(t, true, body["stream"])
}
```

Add imports required by the tests (`encoding/json`, `net/http`, `testing`, and `github.com/stretchr/testify/require`). If the existing implementation has no request helper, the test should call the planned `newLoadtestRequest` signature above.

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
go test ./tools/loadtest -run 'Test(P99LatencyUsesDedicatedHistogram|ChatRequestBodyOmitsStream|StreamRequestBodyEnablesStream)' -v
```

Expected: FAIL because `latencySamples`, `p99Latency`, and `newLoadtestRequest` do not yet exist.

- [ ] **Step 3: Implement the smallest mode-aware change**

In `tools/loadtest/main.go`:

1. Add:

```go
var mode = flag.String("mode", "stream", "request mode: stream or chat")
```

2. Extend `metrics` with:

```go
latencySamples map[int64]int64
```

Initialize it beside `samples`.

3. Extract request construction:

```go
func newLoadtestRequest(base, groupKey, requestMode string) *http.Request {
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	if requestMode == "stream" {
		body = `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	}
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+*key)
	req.Header.Set("Content-Type", "application/json")
	return req
}
```

The helper must use the existing `addr` and `key` flags for compatibility; its `base` and `groupKey` parameters are used to make the test deterministic, so prefer a helper that accepts explicit values and set headers from those arguments rather than reading globals:

```go
func newLoadtestRequest(base, groupKey, requestMode string) *http.Request {
	// ...
	req.Header.Set("Authorization", "Bearer "+groupKey)
	// ...
}
```

4. In `doRequest`, call `newLoadtestRequest(*addr, *key, *mode)` and branch after the HTTP status check:

```go
if *mode == "chat" {
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		m.errs.Add(1)
		m.total.Add(1)
		m.addErr("read:" + err.Error())
		return
	}
	_ = b
	latency := time.Since(reqStart).Milliseconds()
	m.firstByteMS.Add(latency) // only as an internal sum until result formatting is split
	storeLatencySample(m, latency)
	m.total.Add(1)
	return
}
```

Use a separate `latencyMS` atomic sum instead of reusing `firstByteMS`; the final implementation must not label chat full-response latency as first-byte latency. For stream mode, retain the existing `firstByteMS` logic and SSE drain unchanged.

5. Add:

```go
func storeLatencySample(m *metrics, v int64) {
	m.mu.Lock()
	m.latencySamples[v/10]++
	m.mu.Unlock()
}

func p99Latency(m *metrics) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return p99Buckets(m.latencySamples)
}
```

Refactor the existing p99 bucket walk into a small shared helper only if this does not alter stream behavior.

6. Validate the mode at startup. If mode is not `stream` or `chat`, print an error to stderr and exit nonzero before starting workers.

7. Emit mode-specific result output:

```text
mode=stream
avg_first_byte_ms=...
p99_first_byte_ms=...
```

or:

```text
mode=chat
avg_latency_ms=...
p99_latency_ms=...
```

Keep `total`, `errs`, `elapsed`, `concurrency`, and `err_detail` unchanged.

- [ ] **Step 4: Run focused tests and verify GREEN**

Run:

```bash
go test ./tools/loadtest -run 'Test(P99LatencyUsesDedicatedHistogram|ChatRequestBodyOmitsStream|StreamRequestBodyEnablesStream)' -v
go test ./tools/loadtest
```

Expected: PASS with no failures.

- [ ] **Step 5: Run repository verification**

Run:

```bash
go test ./...
go vet ./...
golangci-lint run ./...
```

Expected: all packages pass, vet has no output, and lint exits 0.

- [ ] **Step 6: Commit the implementation**

```bash
git add tools/loadtest/main.go tools/loadtest/main_test.go
git commit -m "feat: add non-stream loadtest mode"
```

---

### Task 2: Run capped non-stream benchmark and collect evidence

**Files:**
- Modify: `docs/superpowers/plans/loadtest-results.md` (only after remote runs finish; anonymous environment details only)
- Create locally ignored temporary files under `/tmp/gpm-bench/` only; never add them to Git.

**Interfaces:**
- Uses the committed `tools/loadtest` binary with `-mode chat`.
- Uses existing `tools/fakeupstream` non-stream chat response.
- Uses remote Docker PostgreSQL and admin setup, but no server identity is copied into results.

- [ ] **Step 1: Cross-compile and upload temporary binaries**

Build locally:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/gpm-bench/server ./cmd/server
go build -o /tmp/gpm-bench/loadtest ./tools/loadtest
go build -o /tmp/gpm-bench/fakeupstream ./tools/fakeupstream
```

Upload binaries and a generated config with random admin token to a remote `/tmp` directory. Do not put the remote address in any repository file or output artifact that will be committed.

- [ ] **Step 2: Start benchmark components under the 50% CPU cap**

On the remote host:

```bash
export GOMAXPROCS=12
taskset -cp 0-11 <server-pid>
taskset -cp 0-11 <fakeupstream-pid>
docker update --cpuset-cpus 0-11 gpm-pg
```

Start fakeupstream with its existing non-stream response and start the server. Confirm `/healthz` responds before load.

- [ ] **Step 3: Initialize isolated benchmark data**

Create one template pointing to fakeupstream, two active accounts with high `max_concurrency`, and one group. Store the one-time group key only in the remote `/tmp` directory. Confirm one non-stream request returns HTTP 200 and a complete JSON response.

- [ ] **Step 4: Run the non-stream ladder**

Run `-mode chat` at concurrency 500, 1000, 2000, and 5000, using 45 seconds timed duration plus 15–20 seconds warmup. For each run capture:

- `mode=chat`, total, errs, avg latency, P99 latency;
- gateway `/healthz` samples;
- `ps` CPU/RSS for gateway and fakeupstream;
- Docker PG CPU/RSS;
- no more than 12 CPUs used by benchmark components.

Only run 10000 if 5000 has zero errors and production CPU remains within the 50% reservation. Stop immediately if the cap or production headroom is threatened.

- [ ] **Step 5: Collect Go pprof without exceeding the cap**

If function-level diagnosis is needed, use temporary loopback-only `net/http/pprof` builds and collect gateway/fakeupstream CPU profiles during a chat run. Do not commit the temporary profiling source or binaries. The final report must distinguish measured data from interpretation.

- [ ] **Step 6: Update anonymous result record and clean up**

Update `docs/superpowers/plans/loadtest-results.md` with a non-stream section containing only anonymous environment data (for example: Linux, 24 logical CPUs, benchmark cap 12 CPUs, PostgreSQL in Docker). Include the exact mode, concurrency, duration, total, errors, avg/P99 latency, and CPU observations. Do not write IP, hostname, SSH address, admin token, or group key.

Stop benchmark processes and remove the temporary remote Docker PostgreSQL container and `/tmp` directory after results are collected. Keep the result commit separate:

```bash
git add docs/superpowers/plans/loadtest-results.md
git commit -m "docs: record capped non-stream loadtest results"
```
