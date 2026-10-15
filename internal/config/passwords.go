package config

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Password hashing
//
// Passwords are stored as PBKDF2-HMAC-SHA256 in the form
//
//	pbkdf2-sha256$<iterations>$<salt base64>$<hash base64>
//
// PBKDF2 comes from the standard library's primitives rather than an
// external module so that the panel keeps its single dependency. The
// iteration count is stored with the hash, so it can be raised later
// without invalidating existing passwords.

const (
	pbkdf2Iterations = 210_000
	pbkdf2SaltLen    = 16
	pbkdf2KeyLen     = 32
	pbkdf2Prefix     = "pbkdf2-sha256"
)

// HashPassword returns a storable representation of the given password.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("config: refusing to hash an empty password")
	}
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := pbkdf2Key([]byte(password), salt, pbkdf2Iterations, pbkdf2KeyLen)
	return fmt.Sprintf("%s$%d$%s$%s", pbkdf2Prefix, pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether the password matches the stored value.
// It accepts either a PBKDF2 hash or, for the convenience of a first run,
// a plain password written directly into the configuration file. Plain
// passwords are compared in constant time too, and the panel logs a
// warning about them at startup.
func VerifyPassword(stored, password string) bool {
	if stored == "" {
		return false
	}
	if !strings.HasPrefix(stored, pbkdf2Prefix+"$") {
		return subtle.ConstantTimeCompare([]byte(stored), []byte(password)) == 1
	}
	parts := strings.Split(stored, "$")
	if len(parts) != 4 {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations <= 0 || iterations > 10_000_000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got := pbkdf2Key([]byte(password), salt, iterations, len(want))
	return subtle.ConstantTimeCompare(want, got) == 1
}

// IsHashed reports whether a stored password is a hash rather than a
// plain value, which the startup log uses to nag about plain ones.
func IsHashed(stored string) bool {
	return strings.HasPrefix(stored, pbkdf2Prefix+"$")
}

// pbkdf2Key implements PBKDF2 as described in RFC 8018 section 5.2. The
// standard library exposes the same algorithm under x/crypto, which this
// project deliberately avoids depending on.
func pbkdf2Key(password, salt []byte, iterations, keyLen int) []byte {
	hashLen := sha256.Size
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	buf := make([]byte, 4)

	for block := 1; block <= blocks; block++ {
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		mac.Write(buf)
		u := mac.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)

		for i := 1; i < iterations; i++ {
			mac.Reset()
			mac.Write(u)
			u = mac.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}
