/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package postgres

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/xdg-go/stringprep"
)

const (
	// scramIterations matches PostgreSQL's default scram_iterations.
	scramIterations = 4096
	// scramSaltLen matches PostgreSQL's SCRAM_DEFAULT_SALT_LEN.
	scramSaltLen = 16
	// scramPrefix starts every SCRAM-SHA-256 verifier PostgreSQL stores.
	scramPrefix = "SCRAM-SHA-256$"
)

// IsPreHashedPassword reports whether PostgreSQL would treat password as an
// already encrypted password (a SCRAM-SHA-256 verifier or an MD5 hash) and
// store it as-is instead of hashing it.
func IsPreHashedPassword(password string) bool {
	if strings.HasPrefix(password, scramPrefix) {
		return true
	}
	if len(password) != 35 || !strings.HasPrefix(password, "md5") {
		return false
	}
	for _, c := range password[3:] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// saslPrep prepares password like PostgreSQL's pg_saslprep does before
// hashing it: pure ASCII is used unchanged; other input is normalized with
// SASLprep (RFC 4013), and input SASLprep rejects is used unchanged.
func saslPrep(password string) string {
	ascii := true
	for i := 0; i < len(password); i++ {
		if password[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return password
	}
	prepped, err := stringprep.SASLprep.Prepare(password)
	if err != nil || prepped == "" {
		return password
	}
	return prepped
}

// ScramSHA256Verifier returns the SCRAM-SHA-256 verifier ("secret") that
// PostgreSQL would store for password, with a random salt and PostgreSQL's
// default iteration count. Sending the verifier instead of the password means
// the plaintext never reaches the server (or its logs). PostgreSQL stores a
// pre-computed verifier as-is, whatever password_encryption is set to.
func ScramSHA256Verifier(password string) (string, error) {
	salt := make([]byte, scramSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("failed to generate SCRAM salt: %w", err)
	}
	return scramSHA256Verifier(password, salt, scramIterations)
}

func scramSHA256Verifier(password string, salt []byte, iterations int) (string, error) {
	salted, err := pbkdf2.Key(sha256.New, saslPrep(password), salt, iterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("failed to derive SCRAM key: %w", err)
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, "Server Key")
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("%s%d:%s$%s:%s", scramPrefix, iterations, b64(salt), b64(storedKey[:]), b64(serverKey)), nil
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
