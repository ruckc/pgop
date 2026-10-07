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

package controller

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// clientTLS is how clients, and the operator itself, connect to a Cluster.
type clientTLS struct {
	// SSLMode is the libpq sslmode.
	SSLMode string
	// CACert is the CA bundle to verify the server with; only set for
	// verify-full.
	CACert []byte
}

// tlsActive reports whether the Cluster's server is confirmed to serve TLS
// with the certificate from spec.tls.secretName.
func tlsActive(cluster *postgresv1alpha1.Cluster) bool {
	return cluster.Spec.TLS != nil && meta.IsStatusConditionTrue(cluster.Status.Conditions, ConditionTypeTLSReady)
}

// clientTLSFor derives the connection security for a Cluster:
//
//   - spec.tls unset: sslmode=disable, exactly as before TLS support existed;
//   - spec.tls set and TLSReady=True: sslmode=verify-full with the Secret's CA;
//   - spec.tls set but not yet active (the pod is restarting onto TLS, the
//     Secret is invalid, or a rotated certificate is not loaded yet):
//     sslmode=prefer, which works against both the old and the new server.
//
// caPEM is the CA from the TLS Secret and is only used when TLS is active.
func clientTLSFor(cluster *postgresv1alpha1.Cluster, caPEM []byte) clientTLS {
	switch {
	case cluster.Spec.TLS == nil:
		return clientTLS{SSLMode: postgres.SSLModeDisable}
	case !tlsActive(cluster):
		return clientTLS{SSLMode: postgres.SSLModePrefer}
	default:
		return clientTLS{SSLMode: postgres.SSLModeVerifyFull, CACert: caPEM}
	}
}

// clusterClientTLS is clientTLSFor, reading the CA from the Cluster's TLS
// Secret when TLS is active.
func clusterClientTLS(ctx context.Context, c client.Reader, cluster *postgresv1alpha1.Cluster) (clientTLS, error) {
	if !tlsActive(cluster) {
		return clientTLSFor(cluster, nil), nil
	}
	name := cluster.Spec.TLS.SecretName
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, secret); err != nil {
		return clientTLS{}, fmt.Errorf("failed to get TLS Secret %q: %w", name, err)
	}
	ca := secret.Data[TLSSecretKeyCA]
	if len(ca) == 0 {
		return clientTLS{}, fmt.Errorf("TLS Secret %q has no %q", name, TLSSecretKeyCA)
	}
	return clientTLSFor(cluster, ca), nil
}

// operatorConnectionConfig builds the operator's (superuser) connection to
// dbName on the Cluster, using the Cluster credentials Secret and the TLS
// settings from clusterClientTLS.
func operatorConnectionConfig(ctx context.Context, c client.Reader, cluster *postgresv1alpha1.Cluster, dbName string) (postgres.ConnectionConfig, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: cluster.Status.SecretName, Namespace: cluster.Namespace}, secret); err != nil {
		return postgres.ConnectionConfig{}, fmt.Errorf("failed to get credentials secret: %w", err)
	}
	t, err := clusterClientTLS(ctx, c, cluster)
	if err != nil {
		return postgres.ConnectionConfig{}, err
	}
	return postgres.ConnectionConfig{
		Host:        clusterHost(cluster),
		Port:        clusterPort(cluster),
		User:        string(secret.Data[SecretKeyUsername]),
		Password:    string(secret.Data[SecretKeyPassword]),
		Database:    dbName,
		SSLMode:     t.SSLMode,
		RootCertPEM: t.CACert,
	}, nil
}

// newOperatorClient connects to dbName on the Cluster as the operator.
func newOperatorClient(ctx context.Context, c client.Reader, cluster *postgresv1alpha1.Cluster, dbName string) (*postgres.Client, error) {
	cfg, err := operatorConnectionConfig(ctx, c, cluster, dbName)
	if err != nil {
		return nil, err
	}
	return postgres.NewClient(cfg)
}

// clusterConnectionFingerprint summarises what clients of a Cluster depend
// on: readiness, port and TLS state.
func clusterConnectionFingerprint(c *postgresv1alpha1.Cluster) string {
	return fmt.Sprintf("%t|%d|%t|%t|%s", c.Status.Ready, clusterPort(c), c.Spec.TLS != nil, tlsActive(c), c.Status.TLSSecretHash)
}

// clusterConnectionChanged lets Cluster updates through only when they change
// how clients connect, so Role and Database controllers (which watch
// Clusters) are not re-run on every Cluster status write.
var clusterConnectionChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldC, okOld := e.ObjectOld.(*postgresv1alpha1.Cluster)
		newC, okNew := e.ObjectNew.(*postgresv1alpha1.Cluster)
		if !okOld || !okNew {
			return true
		}
		return clusterConnectionFingerprint(oldC) != clusterConnectionFingerprint(newC)
	},
}

// connectionURI renders a postgresql:// URI. User, password and database are
// percent-encoded. The CA cannot be embedded: clients using verify-full must
// mount ca.crt and add sslrootcert=<path>.
func connectionURI(user, password, host string, port int32, database, sslMode string) string {
	u := url.URL{
		Scheme:   "postgresql",
		User:     url.UserPassword(user, password),
		Host:     net.JoinHostPort(host, strconv.Itoa(int(port))),
		Path:     "/" + database,
		RawQuery: url.Values{"sslmode": {sslMode}}.Encode(),
	}
	return u.String()
}

// applyConnectionInfo sets the connection keys of a credentials Secret
// (host, port, sslmode, uri and, while TLS is active, ca.crt) on data, and
// removes ca.crt when TLS is not active. Other keys (username, password,
// database) are left as they are. It returns whether data changed.
func applyConnectionInfo(data map[string][]byte, host string, port int32, database string, t clientTLS) bool {
	want := map[string][]byte{
		SecretKeyHost:    []byte(host),
		SecretKeyPort:    []byte(strconv.Itoa(int(port))),
		SecretKeySSLMode: []byte(t.SSLMode),
		SecretKeyURI: []byte(connectionURI(string(data[SecretKeyUsername]), string(data[SecretKeyPassword]),
			host, port, database, t.SSLMode)),
	}
	if len(t.CACert) > 0 {
		want[SecretKeyCACert] = t.CACert
	}

	changed := false
	for k, v := range want {
		if cur, ok := data[k]; !ok || !bytes.Equal(cur, v) {
			data[k] = v
			changed = true
		}
	}
	if _, ok := want[SecretKeyCACert]; !ok {
		if _, has := data[SecretKeyCACert]; has {
			delete(data, SecretKeyCACert)
			changed = true
		}
	}
	return changed
}
