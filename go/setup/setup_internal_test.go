package setup

import (
	"crypto/sha256"
	"encoding/hex"
)

// accountOnlyKey is the endpoint key of the per-user install before --state:
// the first eight bytes of the account's SHA-256.
func accountOnlyKey(account string) string {
	sum := sha256.Sum256([]byte(account))
	return hex.EncodeToString(sum[:8])
}
