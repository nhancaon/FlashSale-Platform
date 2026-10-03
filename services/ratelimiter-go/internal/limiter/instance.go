package limiter

import (
	"crypto/rand"
	"encoding/hex"
)

// instanceID distinguishes this process from other service instances that
// share the same Redis, so sorted-set members never collide across them.
func instanceID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b)
}
