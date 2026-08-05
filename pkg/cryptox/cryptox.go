package cryptox

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
)

// HashKey 返回 key 的 SHA-256 十六进制摘要，用于库内比对（不存明文）。
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// NewGroupKey 生成分组客户端 key：raw 只返回一次给调用方展示，hash 入库。
func NewGroupKey() (raw, hash, prefix string) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("cryptox: rand read failed: " + err.Error())
	}
	raw = "gk-" + hex.EncodeToString(b)
	return raw, HashKey(raw), raw[:8]
}

// Equal 常量时间比较两个摘要。
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
