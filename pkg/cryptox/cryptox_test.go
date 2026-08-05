package cryptox

import "testing"

func TestHashKeyDeterministic(t *testing.T) {
	a := HashKey("gk-abc")
	b := HashKey("gk-abc")
	if a != b || a == "gk-abc" {
		t.Fatalf("hash mismatch: %s %s", a, b)
	}
}

func TestNewGroupKey(t *testing.T) {
	raw, hash, prefix := NewGroupKey()
	if len(raw) != 35 || raw[:3] != "gk-" { // gk- + 32 hex
		t.Fatalf("bad raw: %q", raw)
	}
	if HashKey(raw) != hash {
		t.Fatal("hash mismatch")
	}
	if len(prefix) != 8 {
		t.Fatalf("bad prefix: %q", prefix)
	}
}

func TestEqual(t *testing.T) {
	if !Equal("abc", "abc") || Equal("abc", "abd") {
		t.Fatal("Equal wrong")
	}
}
