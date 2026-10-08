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
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"

	"github.com/xdg-go/stringprep"
)

// parseVerifier splits a SCRAM-SHA-256 verifier into salt, StoredKey and
// ServerKey.
func parseVerifier(t *testing.T, v string) (iter string, salt, storedKey, serverKey []byte) {
	t.Helper()
	m := regexp.MustCompile(`^SCRAM-SHA-256\$(\d+):([^$]+)\$([^:]+):(.+)$`).FindStringSubmatch(v)
	if m == nil {
		t.Fatalf("malformed verifier %q", v)
	}
	dec := func(s string) []byte {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("bad base64 %q in %q", s, v)
		}
		return b
	}
	return m[1], dec(m[2]), dec(m[3]), dec(m[4])
}

// TestScramVerifierRFC7677 checks the verifier against the SCRAM-SHA-256
// exchange of RFC 7677 section 3: the server, holding only the verifier, must
// accept the RFC's client proof and produce the RFC's server signature.
func TestScramVerifierRFC7677(t *testing.T) {
	salt, _ := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	v, err := scramSHA256Verifier("pencil", salt, 4096)
	if err != nil {
		t.Fatal(err)
	}
	iter, gotSalt, storedKey, serverKey := parseVerifier(t, v)
	if iter != "4096" || string(gotSalt) != string(salt) {
		t.Fatalf("unexpected iteration count or salt in %q", v)
	}

	authMessage := "n=user,r=rOprNGfwEbeRWgbNEkqO," +
		"r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096," +
		"c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"
	proof, _ := base64.StdEncoding.DecodeString("dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ=")

	clientSignature := hmacSHA256(storedKey, authMessage)
	clientKey := make([]byte, len(proof))
	for i := range proof {
		clientKey[i] = proof[i] ^ clientSignature[i]
	}
	if sum := sha256.Sum256(clientKey); !hmac.Equal(sum[:], storedKey) {
		t.Error("the RFC 7677 client proof does not verify against StoredKey")
	}
	serverSignature := base64.StdEncoding.EncodeToString(hmacSHA256(serverKey, authMessage))
	if serverSignature != "6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=" {
		t.Errorf("server signature = %q, want the RFC 7677 value", serverSignature)
	}
}

func TestScramSHA256Verifier(t *testing.T) {
	a, err := ScramSHA256Verifier(`pa'ss\word`)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ScramSHA256Verifier(`pa'ss\word`)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("verifiers of the same password must use different salts")
	}
	for _, v := range []string{a, b} {
		if !IsPreHashedPassword(v) {
			t.Errorf("%q is not recognized as a SCRAM verifier", v)
		}
		if strings.Contains(v, "pa'ss") || strings.ContainsAny(v, `'\`) {
			t.Errorf("verifier %q contains the password or characters that need quoting", v)
		}
		if _, salt, _, _ := parseVerifier(t, v); len(salt) != scramSaltLen {
			t.Errorf("salt length = %d, want %d", len(salt), scramSaltLen)
		}
	}
}

func TestSASLPrep(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain ascii", "plain ascii"},
		{"ctrl\x01ascii", "ctrl\x01ascii"},                                   // ASCII is used unchanged, like pg_saslprep
		{"no" + string(rune(0x00a0)) + "break", "no break"},                  // non-ASCII space maps to space
		{"soft" + string(rune(0x00ad)) + "hyphen", "softhyphen"},             // mapped to nothing
		{"a" + string(rune(0x200b)) + "b", "a b"},                            // ZERO WIDTH SPACE is a non-ASCII space for pg_saslprep
		{"a" + string(rune(0x200c)) + "b", "ab"},                             // ZERO WIDTH NON-JOINER is mapped to nothing
		{string(rune(0x2163)), "IV"},                                         // NFKC (ROMAN NUMERAL FOUR)
		{"bad\x07" + string(rune(0x00e9)), "bad\x07" + string(rune(0x00e9))}, // prohibited: used unchanged
	}
	for _, tt := range tests {
		if got := saslPrep(tt.in); got != tt.want {
			t.Errorf("saslPrep(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestSASLPrepTableOverlap guards the U+200B special case in saslPrep: it is
// the only character that RFC 3454 lists both as a non-ASCII space (C.1.2,
// mapped to SPACE by pg_saslprep) and as commonly mapped to nothing (B.1,
// which stringprep applies first).
func TestSASLPrepTableOverlap(t *testing.T) {
	var both []rune
	for r := rune(0); r <= 0x10FFFF; r++ {
		if _, inB1 := stringprep.TableB1.Map(r); inB1 && stringprep.TableC1_2.Contains(r) {
			both = append(both, r)
		}
	}
	if len(both) != 1 || both[0] != zeroWidthSpace {
		t.Errorf("characters in both B.1 and C.1.2 = %U, want only U+200B", both)
	}
}

func TestIsPreHashedPassword(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"SCRAM-SHA-256$4096:abc$def:ghi", true},
		{"SCRAM-SHA-256$", true},
		{"md5" + strings.Repeat("0a", 16), true},
		{"md5" + strings.Repeat("0A", 16), false}, // PostgreSQL only treats lower-case hex as MD5
		{"md5" + strings.Repeat("0a", 15), false},
		{"md5" + strings.Repeat("0g", 16), false},
		{"scram-sha-256$x", false},
		{"plain-password", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsPreHashedPassword(tt.in); got != tt.want {
			t.Errorf("IsPreHashedPassword(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
