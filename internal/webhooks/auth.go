package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
)

// hmacSHA256 returns the HMAC-SHA256 of body under secret.
func hmacSHA256(secret, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return mac.Sum(nil)
}

// subtleCompare compares two digests in constant time.
func subtleCompare(expected, provided []byte) int {
	return subtle.ConstantTimeCompare(expected, provided)
}
