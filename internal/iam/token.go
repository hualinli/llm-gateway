package iam

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// newToken 生成 32 字节随机 token，base64url（无填充）编码：43 个字符，
// 可安全放进 Cookie / JSON，且不含需要转义的字符。
func newToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("iam: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashToken 是 token 落库前的唯一变换：SHA-256 十六进制摘要（64 字符）。
// 明文 token 绝不写库、绝不进日志。
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// constantTimeEqual 常量时间比较两个等长摘要；长度不同直接返回 false
// （长度本身不是秘密，摘要长度固定）。
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
