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
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/lib/pq"
)

// buildDSN renders cfg as a libpq key/value connection string. Every value is
// single-quoted with backslashes and single quotes escaped, so passwords or
// names containing spaces, quotes or '=' cannot break or inject into the DSN.
//
// For sslmode=verify-full the CA from RootCertPEM is registered with lib/pq as
// a custom tls.Config and referenced through its "pqgo-<key>" sslmode, because
// lib/pq can only take an inline root certificate together with an inline
// client certificate.
func buildDSN(cfg ConnectionConfig) (string, error) {
	if cfg.SSLMode == "" {
		cfg.SSLMode = defaultSSLMode
	}
	if cfg.Database == "" {
		cfg.Database = defaultDatabase
	}

	sslMode := cfg.SSLMode
	switch cfg.SSLMode {
	case SSLModeDisable, SSLModePrefer, SSLModeRequire:
	case SSLModeVerifyFull:
		key, err := registerVerifyFullTLS(cfg.Host, cfg.RootCertPEM)
		if err != nil {
			return "", err
		}
		sslMode = "pqgo-" + key
	default:
		return "", fmt.Errorf("unsupported sslmode %q", cfg.SSLMode)
	}

	pairs := make([]struct{ k, v string }, 0, 6+len(operatorSessionSettings))
	pairs = append(pairs, []struct{ k, v string }{
		{"host", cfg.Host},
		{"port", strconv.Itoa(int(cfg.Port))},
		{"user", cfg.User},
		{"password", cfg.Password},
		{"dbname", cfg.Database},
		{"sslmode", sslMode},
	}...)
	// Pin the session settings of every operator session (see
	// operatorSessionSettings). Startup parameters take precedence over
	// ALTER DATABASE / ALTER ROLE ... SET.
	pairs = append(pairs, operatorSessionSettings...)
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, p.k+"="+quoteDSNValue(p.v))
	}
	return strings.Join(parts, " "), nil
}

// operatorSearchPath is the search_path of every connection made by NewClient.
// pg_temp is listed last so temporary objects can never shadow catalog ones.
const operatorSearchPath = "pg_catalog, pg_temp"

// operatorSessionSettings are sent as startup parameters on every
// connection made by NewClient. The operator connects as a superuser to
// databases whose owners (Database writers, or anyone with the owner's
// credentials) can set per-database defaults with ALTER DATABASE ... SET.
// Without these pins they could:
//   - search_path: point name resolution at a schema they can write to and
//     shadow functions and operators used by the operator's queries, running
//     code as a superuser;
//   - role: run the operator's session as their own role
//     (ALTER DATABASE ... SET role = 'owner' is accepted for the owner);
//   - statement_timeout, lock_timeout, idle_in_transaction_session_timeout,
//     idle_session_timeout, default_transaction_read_only,
//     check_function_bodies, row_security, default_tablespace,
//     temp_tablespaces: make the operator's statements fail, or behave
//     differently, in their database;
//   - exit_on_error (superuser context, so only with a parameterGrants
//     grant): end the operator's session on any error.
//
// They all exist in every supported PostgreSQL version (14 and later);
// settings that only exist in newer versions are pinned after connecting
// (versionedSessionPins).
var operatorSessionSettings = []struct{ k, v string }{
	{settingSearchPath, operatorSearchPath},
	{settingRole, roleNone},
	{settingStatementTimeout, "0"},
	{"lock_timeout", "0"},
	{"idle_in_transaction_session_timeout", "0"},
	{"idle_session_timeout", "0"},
	{"exit_on_error", settingOff},
	{"default_transaction_read_only", settingOff},
	{"check_function_bodies", "on"},
	{"row_security", "on"},
	{"default_tablespace", ""},
	{settingTempTablespaces, ""},
}

// Names and values of session settings used in more than one place.
const (
	settingSearchPath       = "search_path"
	settingRole             = "role"
	settingStatementTimeout = "statement_timeout"
	settingTempTablespaces  = "temp_tablespaces"
	roleNone                = "none"
	settingOff              = "off"
)

// quoteDSNValue quotes a libpq connection-string value: the value is wrapped
// in single quotes and any backslash or single quote is backslash-escaped.
func quoteDSNValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `'`, `\'`)
	return "'" + v + "'"
}

// registerVerifyFullTLS registers a tls.Config that verifies the server
// certificate chain against caPEM and the hostname against host, and returns
// its lib/pq registry key. The key is derived from (host, caPEM), so repeated
// connections to the same cluster reuse one registry entry and the registry
// only grows with the number of distinct cluster/CA pairs.
func registerVerifyFullTLS(host string, caPEM []byte) (string, error) {
	if len(caPEM) == 0 {
		return "", errors.New("sslmode=verify-full requires a root certificate")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return "", errors.New("failed to parse root certificate PEM")
	}

	h := sha256.New()
	h.Write([]byte(host))
	h.Write([]byte{0})
	h.Write(caPEM)
	key := "pgop" + hex.EncodeToString(h.Sum(nil))[:32]

	if err := pq.RegisterTLSConfig(key, &tls.Config{
		RootCAs:    pool,
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	}); err != nil {
		return "", fmt.Errorf("failed to register TLS config: %w", err)
	}
	return key, nil
}
