package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// 评审定夺①：JWT 密钥 config 强制（GPM_JWT_SECRET），缺失启动失败——
// 随机生成 = 重启全失效 + 多实例不一致。
// 评审定夺②：TTL 24h（用户决策 2026-08-11，原 15min 短时效导致日常使用频繁掉线；
// 账号禁用/降权的即时生效仍靠内存快照广播，长 TTL 仅弱化快照失效后的最终兜底）。
const DefaultTTL = 24 * time.Hour

var (
	ErrInvalidToken = errors.New("auth: invalid token")
	ErrTokenExpired = errors.New("auth: token expired")
)

// Claims JWT 载荷：userID/email/role/exp（HS256）。
type Claims struct {
	UserID int64  `json:"user_id"`
	Email  string `json:"email"`
	Role   string `json:"role"`
	jwt.RegisteredClaims
}

// Issuer JWT 签发/验证器（单实例；HS256）。
type Issuer struct {
	secret []byte
	ttl    time.Duration
}

// NewIssuer 构造签发器（TTL = DefaultTTL 15min）。
func NewIssuer(secret string) *Issuer {
	return &Issuer{secret: []byte(secret), ttl: DefaultTTL}
}

// Issue 签发 HS256 JWT。
func (i *Issuer) Issue(userID int64, email, role string) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID: userID,
		Email:  email,
		Role:   role,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(i.secret)
}

// Verify 验证签名与过期，返回 claims。任何失败（签名错/篡改/过期/算法不符）
// 均返回错误（区分过期以便调用方选择文案）。
func (i *Issuer) Verify(token string) (*Claims, error) {
	parsed, err := jwt.ParseWithClaims(token, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrInvalidToken // 算法替换防护（HS 族以外的算法拒绝）
		}
		return i.secret, nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := parsed.Claims.(*Claims)
	if !ok || !parsed.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}
