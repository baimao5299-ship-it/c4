// SPDX-License-Identifier: AGPL-3.0-or-later
// The ClientHello profile is adapted from is7Qin/sub2api's LGPL-3.0
// tlsfingerprint package. Keeping it isolated preserves the default Go TLS
// path unless the deployment explicitly enables convergence.
package tlsprofile

import (
	"context"
	"fmt"
	"net"
	"time"

	utls "github.com/refraction-networking/utls"
)

// NewSub2Node24DialTLSContext returns a net/http-compatible TLS dialer using
// the stable Node.js 24 ClientHello profile maintained by Sub2API.
func NewSub2Node24DialTLSContext(dial func(context.Context, string, string) (net.Conn, error), handshakeTimeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	return newSub2Node24DialTLSContext(dial, handshakeTimeout, nil)
}

// newSub2Node24DialTLSContext keeps the production path on normal certificate
// verification while allowing a local test server to provide its own trust
// configuration. The config factory is intentionally unexported.
func newSub2Node24DialTLSContext(dial func(context.Context, string, string) (net.Conn, error), handshakeTimeout time.Duration, configFactory func(string) *utls.Config) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		config := &utls.Config{ServerName: host}
		if configFactory != nil {
			if custom := configFactory(host); custom != nil {
				config = custom
			}
		}
		client := utls.UClient(conn, config, utls.HelloCustom)
		if err := client.ApplyPreset(sub2Node24Spec()); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("apply Sub2 TLS profile: %w", err)
		}
		handshakeCtx := ctx
		cancel := func() {}
		if handshakeTimeout > 0 {
			handshakeCtx, cancel = context.WithTimeout(ctx, handshakeTimeout)
		}
		defer cancel()
		if err := client.HandshakeContext(handshakeCtx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("Sub2 TLS handshake: %w", err)
		}
		return client, nil
	}
}

func sub2Node24Spec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		CipherSuites:       []uint16{0x1301, 0x1302, 0x1303, 0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8, 0xc009, 0xc013, 0xc00a, 0xc014, 0x009c, 0x009d, 0x002f, 0x0035},
		CompressionMethods: []uint8{0},
		TLSVersMin:         utls.VersionTLS10,
		TLSVersMax:         utls.VersionTLS13,
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.GREASEEncryptedClientHelloExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519, utls.CurveP256, utls.CurveP384}},
			&utls.SupportedPointsExtension{SupportedPoints: []uint8{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601, 0x0201}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{uint8(utls.PskModeDHE)}},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
		},
	}
}
