// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import "testing"

func TestPprofListenAddressMustBeLoopback(t *testing.T) {
	for _, tc := range []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:6060", true},
		{"localhost:6060", true},
		{"[::1]:6060", true},
		{":6060", false},
		{"0.0.0.0:6060", false},
		{"192.0.2.10:6060", false},
		{"bad-address", false},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			if got := isLoopbackListenAddr(tc.addr); got != tc.ok {
				t.Fatalf("isLoopbackListenAddr(%q) = %v, want %v", tc.addr, got, tc.ok)
			}
		})
	}
}
