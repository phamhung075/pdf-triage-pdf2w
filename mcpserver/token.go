package mcpserver

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// tokenFileName is the TS MCP_TOKEN_FILE basename (mcp-server.ts:493).
const tokenFileName = ".mcp-api-token"

// TokenPath is BASE_DIR/.mcp-api-token.
func TokenPath(baseDir string) string { return filepath.Join(baseDir, tokenFileName) }

// GetOrCreateToken ports getOrCreateMcpApiToken (mcp-server.ts:501). It reads the persisted token
// and returns it; when the file is absent or blank it generates a fresh 24-byte token rendered as
// lowercase hex (the same encoding as Node's crypto.randomBytes(24).toString('hex'), 48 chars)
// and persists it. The bool reports whether the token was newly generated.
func GetOrCreateToken(baseDir string) (token string, created bool, err error) {
	path := TokenPath(baseDir)
	if raw, readErr := os.ReadFile(path); readErr == nil {
		if existing := strings.TrimSpace(string(raw)); existing != "" {
			return existing, false, nil
		}
	}

	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", false, err
	}
	token = hex.EncodeToString(buf[:])
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", false, err
	}
	return token, true, nil
}

// TokenMatches ports the TS authorization check: an equal-length comparison through
// crypto/subtle.ConstantTimeCompare. The explicit length check preserves the TS behavior of never
// calling timingSafeEqual with mismatched buffer lengths (which would throw in Node).
func TokenMatches(presented, token string) bool {
	if len(presented) != len(token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

// extractBearerToken ports the TS parse: a string starting with "Bearer " yields everything after
// the prefix, anything else yields the empty string.
func extractBearerToken(header string) string {
	const prefix = "Bearer "
	if strings.HasPrefix(header, prefix) {
		return header[len(prefix):]
	}
	return ""
}
