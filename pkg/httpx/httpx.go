// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package httpx 构造共享的上游 HTTP 客户端（规格 §10.2 连接层参数）。
package httpx

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/is7qin/c3api/pkg/tlsprofile"
)

type TransportConfig struct {
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	// MaxConnsPerHost 单 host 连接总数上限（含在用+空闲；0 = 不限）。
	// 网关既有 client 保持 0 不限（压测验证形态）；SDK 适配层装配显式上界。
	MaxConnsPerHost     int
	IdleConnTimeout     time.Duration
	DialTimeout         time.Duration
	TLSHandshakeTimeout time.Duration
	ForceHTTP2          bool
	TLSConvergence      bool
	// Proxy 上游请求代理函数（nil = 直连，默认）。不再隐式装配
	// http.ProxyFromEnvironment——HTTP_PROXY 设了会静默改道全部上游请求
	//（含 x-api-key/Authorization 凭据，WS 升级大概率失败），压测行为随
	// 环境漂移。装配方（main NewClient / SetTransport 两处）显式决定。
	Proxy func(*http.Request) (*url.URL, error)
	// DialContext optionally replaces the direct dialer (used by socks5h).
	// It is deliberately explicit; environment proxy variables are ignored.
	DialContext func(context.Context, string, string) (net.Conn, error)
}

func NewTransport(cfg TransportConfig) *http.Transport {
	dialer := &net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: 30 * time.Second}
	handshakeTimeout := cfg.TLSHandshakeTimeout
	if handshakeTimeout <= 0 {
		handshakeTimeout = 10 * time.Second
	}
	dialContext := cfg.DialContext
	if dialContext == nil {
		dialContext = dialer.DialContext
	}
	transport := &http.Transport{
		Proxy:                 cfg.Proxy, // nil = 直连（防环境代理劫持 + 压测确定性）；装配方显式决定
		DialContext:           dialContext,
		ForceAttemptHTTP2:     cfg.ForceHTTP2,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		MaxConnsPerHost:       cfg.MaxConnsPerHost,
		IdleConnTimeout:       cfg.IdleConnTimeout,
		TLSHandshakeTimeout:   handshakeTimeout,
		ExpectContinueTimeout: time.Second,
	}
	if cfg.TLSConvergence {
		transport.ForceAttemptHTTP2 = false
		transport.DialTLSContext = tlsprofile.NewSub2Node24DialTLSContext(dialContext, handshakeTimeout)
	}
	return transport
}

func NewClient(cfg TransportConfig) *http.Client {
	return &http.Client{Transport: NewTransport(cfg)}
}
