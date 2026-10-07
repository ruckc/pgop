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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
)

const testHost = "db.ns.svc.cluster.local"

func testCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestQuoteDSNValue(t *testing.T) {
	tests := map[string]string{
		"plain":        `'plain'`,
		"":             `''`,
		"with space":   `'with space'`,
		`it's`:         `'it\'s'`,
		`back\slash`:   `'back\\slash'`,
		`x' sslmode=a`: `'x\' sslmode=a'`,
	}
	for in, want := range tests {
		if got := quoteDSNValue(in); got != want {
			t.Errorf("quoteDSNValue(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildDSNRoundTrip parses the generated DSN with lib/pq itself, so values
// with spaces, quotes, backslashes or embedded key=value pairs must come back
// unchanged and cannot inject other settings.
func TestBuildDSNRoundTrip(t *testing.T) {
	cfg := ConnectionConfig{
		Host:     testHost,
		Port:     6432,
		User:     `we'ird user`,
		Password: `p a\ss' sslmode=disable host=evil`,
		Database: "my db",
	}
	dsn, err := buildDSN(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pq.NewConfig(dsn)
	if err != nil {
		t.Fatalf("lib/pq could not parse %q: %v", dsn, err)
	}
	if got.Host != cfg.Host || got.Port != 6432 || got.User != cfg.User || got.Password != cfg.Password || got.Database != cfg.Database {
		t.Errorf("round trip mismatch: got host=%q port=%d user=%q password=%q dbname=%q",
			got.Host, got.Port, got.User, got.Password, got.Database)
	}
	if got.SSLMode != pq.SSLModeDisable {
		t.Errorf("sslmode = %q, want the default %q", got.SSLMode, pq.SSLModeDisable)
	}
}

func TestBuildDSNDefaults(t *testing.T) {
	dsn, err := buildDSN(ConnectionConfig{Host: testHost, Port: 5432, User: testPGUser})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"dbname='postgres'", "sslmode='disable'"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("dsn %q does not contain %q", dsn, want)
		}
	}
}

func TestBuildDSNSSLModes(t *testing.T) {
	for _, mode := range []string{SSLModeDisable, SSLModePrefer, SSLModeRequire} {
		dsn, err := buildDSN(ConnectionConfig{Host: testHost, SSLMode: mode})
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if !strings.Contains(dsn, "sslmode='"+mode+"'") {
			t.Errorf("dsn %q does not use sslmode %s", dsn, mode)
		}
	}
	if _, err := buildDSN(ConnectionConfig{Host: testHost, SSLMode: "verify-ca"}); err == nil {
		t.Error("expected an error for an unsupported sslmode")
	}
}

func TestBuildDSNVerifyFull(t *testing.T) {
	if _, err := buildDSN(ConnectionConfig{Host: testHost, SSLMode: SSLModeVerifyFull}); err == nil {
		t.Error("verify-full without a root certificate must fail")
	}
	if _, err := buildDSN(ConnectionConfig{Host: testHost, SSLMode: SSLModeVerifyFull, RootCertPEM: []byte("junk")}); err == nil {
		t.Error("verify-full with an unparsable root certificate must fail")
	}

	ca := testCAPEM(t)
	cfg := ConnectionConfig{Host: testHost, User: testPGUser, Password: testPGPassword, SSLMode: SSLModeVerifyFull, RootCertPEM: ca}
	dsn, err := buildDSN(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pq.NewConfig(dsn)
	if err != nil {
		t.Fatalf("lib/pq could not parse %q: %v", dsn, err)
	}
	if !strings.HasPrefix(string(parsed.SSLMode), "pqgo-pgop") {
		t.Errorf("sslmode = %q, want a registered pqgo-pgop… TLS config", parsed.SSLMode)
	}

	// The same cluster and CA reuse one registry entry; another CA does not.
	dsn2, err := buildDSN(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dsn2 != dsn {
		t.Errorf("expected a stable DSN for the same host and CA")
	}
	cfg.RootCertPEM = testCAPEM(t)
	dsn3, err := buildDSN(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dsn3 == dsn {
		t.Errorf("expected a different TLS config for a different CA")
	}
}
