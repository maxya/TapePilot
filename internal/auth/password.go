// Package auth protects the administrative HTTP surface and local credentials.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/maxya/TapePilot/internal/store"
	"golang.org/x/crypto/argon2"
	"strings"
	"unicode/utf8"
)

var ErrPassword = errors.New("password must contain 12 through 1024 UTF-8 bytes")

const passwordMemory = 64 * 1024
const passwordTime = 3
const passwordThreads = 2

func PasswordHash(password string) (string, error) {
	if len(password) < 12 || len(password) > 1024 || !utf8.ValidString(password) {
		return "", ErrPassword
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, passwordTime, passwordMemory, passwordThreads, 32)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=2$%s$%s", enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}
func PasswordMatches(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=65536,t=3,p=2" || len(password) > 1024 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != 16 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(expected) != 32 {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, passwordTime, passwordMemory, passwordThreads, 32)
	return subtle.ConstantTimeCompare(expected, actual) == 1
}

// Bootstrap and Reset are local setup helpers. HTTP routes never expose them.
func Bootstrap(ctx context.Context, db *store.Store, password string) error {
	hash, err := PasswordHash(password)
	if err != nil {
		return err
	}
	return db.SetAdminPassword(ctx, hash, false)
}
func Reset(ctx context.Context, db *store.Store, password string) error {
	hash, err := PasswordHash(password)
	if err != nil {
		return err
	}
	return db.SetAdminPassword(ctx, hash, true)
}
