# Third-Party Notices

## Sub2API TLS profile

`pkg/tlsprofile/sub2_node24.go` contains a Go adaptation of the Node.js 24
ClientHello profile published in
[is7Qin/sub2api](https://github.com/is7qin/sub2api), licensed under the
GNU Lesser General Public License v3.0 (LGPL-3.0).

The profile is isolated behind the optional `codex_tls_convergence_enabled`
setting. It is disabled by default and only applies to Codex SDK upstream
connections. The rest of C3API keeps its standard Go TLS transport.

The `github.com/refraction-networking/utls` dependency remains subject to its
upstream license and copyright notices.
