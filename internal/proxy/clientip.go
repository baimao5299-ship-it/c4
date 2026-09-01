// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"net"
	"net/http"
	"net/textproto"
	"strings"
)

// clientIPHeaders 供应商/反代头名（init 时 CanonicalMIMEHeaderKey 归一）：Header.Get
// 对非规范键（如 "CF-Connecting-IP"）每次调用都走 canonicalMIMEHeaderKey 规范化
// 路径恒分配（本三头不在 commonHeader 常用头缓存内——缓存仅含 ~40 个常见头如
// Accept/Content-Type 等常见头，CF-Connecting-IP/True-Client-IP/X-Real-IP/
// X-Forwarded-For 本就不在其中）；规范键（"Cf-Connecting-Ip"）走 quick-check 零分配
// 直返——init 归一使请求路径零分配。HTTP/1.1 与 HTTP/2 入站请求头均以规范形
// 存储（textproto 归一），Get(规范键) 与 Get(任意大小写) 语义等价——零分配
// 且不牺牲查找正确性。AllocsPerRun==0 测试钉住。
var clientIPHeaders = func() []string {
	canon := func(s string) string { return textproto.CanonicalMIMEHeaderKey(s) }
	return []string{canon("CF-Connecting-IP"), canon("True-Client-IP"), canon("X-Real-IP"), canon("X-Forwarded-For")}
}()

var clientIPSources = [...]string{"cf_connecting_ip", "true_client_ip", "x_real_ip", "x_forwarded_for"}

const clientIPSourceRemoteAddr = "remote_addr"

// clientIP 提取客户端 IP（审计/排障的尽力而为标识，非安全边界——不做鉴权/
// 限流决策依据；spec 2026-08-17 用户裁决方案：供应商头识别 + RemoteAddr 兜底）：
//   - behindCDN=false（默认）：完全不读供应商头（零伪造面，与直连行为一致），
//     直取 RemoteAddr 剥端口（IPv6 [::1]:port → ::1；net.SplitHostPort 失败
//     原样返回）。
//   - behindCDN=true：若配置 trusted_proxy_cidrs，先确认直接对端命中可信网段，
//     再按序采信首个非空头 CF-Connecting-IP → True-Client-IP → X-Real-IP →
//     X-Forwarded-For。X-Forwarded-For 按逗号链从左到右取首个合法 IP（支持
//     常见的 IPv6 方括号写法）；所有头均 TrimSpace，值域截断 64 字符——真实
//     IP 最长 IPv6 45 字符。不匹配时忽略这些头并返回 RemoteAddr。未配置网段
//     时沿用只对 CDN/反向代理暴露的防火墙部署契约。全空 → RemoteAddr 剥端口。
//
// 热路径零分配（≤64 路径）：Header.Get map 查找、TrimSpace 子串、切片、
// SplitHostPort 均零分配；>64 防御截断必须 strings.Clone（string(s[:64]) 在
// string 操作数上是恒等转换零拷贝——防 retain 意图静默失败；Clone 才真正分配
// 新串，防 retain 巨型头值直到日志 flush——客户端可塞 1MB 头值，切片引用会
// retain 整个头 map 值；64 字节小分配走大小类，GC 廉价）。AllocsPerRun==0
// 测试钉住。
func clientIP(r *http.Request, behindCDN bool) string {
	value, _, _ := clientIPDetails(r, behindCDN)
	return value
}

// clientIPDetails extends clientIP with request-time provenance. A direct
// remote peer is trusted when the gateway is not configured behind a CDN. In
// CDN mode, a recognized forwarded header is trusted by that explicit
// deployment contract; falling back to RemoteAddr is marked untrusted because
// it most likely identifies the proxy rather than the end client.
func clientIPDetails(r *http.Request, behindCDN bool) (value, source string, trusted bool) {
	return clientIPDetailsWithTrustedProxies(r, behindCDN, nil)
}

// clientIPDetailsWithTrustedProxies is the request-time attribution path used
// by production proxies. When trustedProxyCIDRs is configured, forwarded
// identity headers are accepted only from a peer inside those networks. An
// empty list intentionally preserves the historical BehindCDN behavior for
// deployments that enforce the same boundary outside the application.
func clientIPDetailsWithTrustedProxies(r *http.Request, behindCDN bool, trustedProxyCIDRs []*net.IPNet) (value, source string, trusted bool) {
	if behindCDN {
		if len(trustedProxyCIDRs) > 0 && !trustedProxyPeer(r.RemoteAddr, trustedProxyCIDRs) {
			return stripPort(r.RemoteAddr), clientIPSourceRemoteAddr, false
		}
		for i, name := range clientIPHeaders {
			v := strings.TrimSpace(r.Header.Get(name))
			if v == "" {
				continue
			}
			if i == len(clientIPHeaders)-1 {
				if forwarded := firstForwardedIP(v); forwarded != "" {
					return forwarded, clientIPSources[i], true
				}
				continue
			}
			return truncateClientIP(v), clientIPSources[i], true
		}
		return stripPort(r.RemoteAddr), clientIPSourceRemoteAddr, false
	}
	return stripPort(r.RemoteAddr), clientIPSourceRemoteAddr, true
}

// firstForwardedIP extracts the client-most address from a conventional
// X-Forwarded-For chain. It deliberately accepts only syntactically valid IP
// literals, so a malformed token cannot become an apparently precise audit
// identity. The caller has already established that the immediate peer is a
// trusted reverse proxy.
func firstForwardedIP(value string) string {
	for _, raw := range strings.Split(value, ",") {
		token := strings.TrimSpace(raw)
		if token == "" {
			continue
		}
		if strings.HasPrefix(token, "[") {
			if end := strings.IndexByte(token, ']'); end > 1 {
				host := token[1:end]
				if net.ParseIP(host) != nil {
					return truncateClientIP(host)
				}
			}
		}
		if net.ParseIP(token) != nil {
			return truncateClientIP(token)
		}
		if host, _, err := net.SplitHostPort(token); err == nil && net.ParseIP(host) != nil {
			return truncateClientIP(host)
		}
	}
	return ""
}

// parseTrustedProxyCIDRs compiles configuration once during proxy
// construction, keeping CIDR parsing out of the request hot path. Config.Load
// validates these values before main constructs the proxy; invalid values are
// ignored here defensively so tests and embedders retain a non-panicking New.
func parseTrustedProxyCIDRs(raw []string) []*net.IPNet {
	if len(raw) == 0 {
		return nil
	}
	networks := make([]*net.IPNet, 0, len(raw))
	for _, value := range raw {
		_, network, err := net.ParseCIDR(strings.TrimSpace(value))
		if err == nil && network != nil {
			networks = append(networks, network)
		}
	}
	return networks
}

func trustedProxyPeer(remoteAddr string, networks []*net.IPNet) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, network := range networks {
		if network != nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

// truncateClientIP 值域截断（>64 必须 strings.Clone——切片引用会 retain 整个
// 头 map 值；正常 IP 恒 ≤64 直返零分配）。
func truncateClientIP(s string) string {
	if len(s) > 64 {
		return strings.Clone(s[:64])
	}
	return s
}

// stripPort RemoteAddr 剥端口（IPv4 1.2.3.4:8080 → 1.2.3.4；IPv6
// [::1]:8080 → ::1；无端口/畸形 → 原样返回——SplitHostPort 失败不丢值）。
func stripPort(s string) string {
	if host, _, err := net.SplitHostPort(s); err == nil {
		return host
	}
	return s
}
