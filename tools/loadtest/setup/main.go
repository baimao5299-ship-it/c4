// setup 构造多租户压测数据（Phase 3a 数据模型）：模板（三格式）→ 公开组 →
// 账号（分散模板/组）→ 用户 → 逐个登录建 key，key 明文写文件（loadtest -keys 用）。
//
// 用法: go run ./tools/loadtest/setup \
//   -addr http://127.0.0.1:8080 -admin-token <GPM_ADMIN_TOKEN> \
//   -upstream http://127.0.0.1:9100 \
//   -users 5000 -accounts 5000 -groups 20 -keys-out keys.txt
//
// 说明：
//   - 模板 base_url = -upstream（裸根约定：不含 /v1，服务端校验会拒绝尾 /v1）
//   - 组全部 public（key 可选性无限制）；账号 upstream_key 统一 "sk-upstream"
//   - 用户密码统一 "loadtest-pass-1"（bcrypt 校验可验证）
//   - key 不设 quota / max_concurrency（0 = 不限，热路径零成本，压测纯转发路径）
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

var (
	addr       = flag.String("addr", "http://127.0.0.1:8080", "gateway base url")
	adminToken = flag.String("admin-token", "", "GPM_ADMIN_TOKEN (admin API auth)")
	upstream   = flag.String("upstream", "http://127.0.0.1:9100", "fake upstream base url (bare root)")
	users      = flag.Int("users", 5000, "number of users (each gets one key)")
	accounts   = flag.Int("accounts", 5000, "number of upstream accounts")
	groups     = flag.Int("groups", 20, "number of public groups")
	keysOut    = flag.String("keys-out", "keys.txt", "output file with one key per line")
	workers    = flag.Int("workers", 64, "parallelism for user/key creation (bcrypt heavy)")
)

const (
	pass = "loadtest-pass-1"
	user = "user%d@loadtest.test"
)

// 响应解析用最小结构（JSON 字段名 = Go 字段名 / openapi tag，见 api.gen.go）。
type tpl struct{ ID int64 }
type grp struct{ ID int64 }
type acc struct{ ID int64 }
type usr struct{ ID int64 }
type loginResp struct {
	Token string `json:"token"`
}
type keyResp struct {
	Key string `json:"key"`
}

func main() {
	flag.Parse()
	if *adminToken == "" {
		fmt.Fprintln(os.Stderr, "-admin-token required (GPM_ADMIN_TOKEN)")
		os.Exit(2)
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	// call 发请求：auth = 完整 "Authorization" 头值（"" = 不带头；登录公开）。
	call := func(method, path, auth string, body any, out any) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, *addr+path, rd)
		must(err)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := hc.Do(req)
		must(err)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			fmt.Fprintf(os.Stderr, "%s %s → %d: %s\n", method, path, resp.StatusCode, b)
			os.Exit(1)
		}
		if out != nil {
			must(json.Unmarshal(b, out))
		}
	}
	admin := func(method, path string, body any, out any) {
		call(method, path, "Bearer "+*adminToken, body, out)
	}

	// 1) 模板（三格式 × 每格式 2 个 = 多模板；base_url 裸根）
	tplIDs := make([]int64, 0, 6)
	for _, t := range []struct{ name, format, model string }{
		{"chat", "openai-chat", "gpt-4o"},
		{"chat-b", "openai-chat", "gpt-4o"},
		{"responses", "openai-responses", "gpt-4o"},
		{"responses-b", "openai-responses", "gpt-4o"},
		{"anthropic", "anthropic", "claude-3-5-sonnet-20241022"},
		{"anthropic-b", "anthropic", "claude-3-5-sonnet-20241022"},
	} {
		var out tpl
		admin(http.MethodPost, "/admin/templates", map[string]any{
			"name": "tpl-" + t.name, "base_url": *upstream,
			"supported_formats": []string{t.format}, "models": []string{t.model},
		}, &out)
		tplIDs = append(tplIDs, out.ID)
	}
	fmt.Printf("templates: %v\n", tplIDs)

	// 2) 公开组 ×N
	groupIDs := make([]int64, 0, *groups)
	for i := 0; i < *groups; i++ {
		var out grp
		admin(http.MethodPost, "/admin/groups", map[string]any{
			"name": fmt.Sprintf("pool-%d", i), "visibility": "public",
		}, &out)
		groupIDs = append(groupIDs, out.ID)
	}
	fmt.Printf("groups: %d\n", len(groupIDs))

	// 3) 账号 ×N：模板/组轮流分配
	for i := 0; i < *accounts; i++ {
		var out acc
		admin(http.MethodPost, "/admin/accounts", map[string]any{
			"name":         fmt.Sprintf("acc-%d", i),
			"template_id":  tplIDs[i%len(tplIDs)],
			"upstream_key": "sk-upstream",
			"group_ids":    []int64{groupIDs[i%len(groupIDs)]},
			"weight":       100, "max_concurrency": 0,
		}, &out)
	}
	fmt.Printf("accounts: %d\n", *accounts)

	// 4) 用户 ×N + 5) 逐个登录建 key（bcrypt 重，并行 worker）
	keysFile, err := os.Create(*keysOut)
	must(err)
	defer keysFile.Close()
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, *workers)
	var created int64
	for i := 0; i < *users; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			email := fmt.Sprintf(user, i)
			var u usr
			admin(http.MethodPost, "/admin/users", map[string]any{
				"email": email, "password": pass,
			}, &u)
			var lr loginResp
			call(http.MethodPost, "/user/auth/login", "", map[string]any{
				"email": email, "password": pass,
			}, &lr)
			var kr keyResp
			call(http.MethodPost, "/user/keys", "Bearer "+lr.Token, map[string]any{
				"name": "load", "group_id": groupIDs[i%len(groupIDs)],
			}, &kr)
			mu.Lock()
			fmt.Fprintln(keysFile, kr.Key)
			created++
			if created%1000 == 0 {
				fmt.Printf("keys: %d\n", created)
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	fmt.Printf("done: users=%d keys=%d → %s\n", *users, created, *keysOut)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "setup:", err)
		os.Exit(1)
	}
}
