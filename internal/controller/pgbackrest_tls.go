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
	"context"
	"crypto/x509"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

// pgBackRest TLS.
//
// The pgBackRest TLS server in the Cluster pod and the backup Jobs
// authenticate each other with certificates from a CA the operator creates
// per Cluster while it has physical backups, independently of spec.tls (the
// PostgreSQL server certificate may come from a user Secret or cert-manager,
// whose CA key pgop does not hold, and pgBackRest needs a client certificate
// as well). The Secret "<cluster>-pgbackrest-tls" holds:
//
//	ca.crt, ca.key          the CA (ca.key is never mounted)
//	tls.crt, tls.key        the server certificate, for the Service FQDN
//	client.crt, client.key  the client certificate (CN pgbackrestClientCN)
//
// The sidecar restarts the TLS server when its mounted certificates change,
// and every Job pod reads the current client certificate at start.
const (
	pgbackrestTLSKeyCAKey      = "ca.key"
	pgbackrestTLSKeyClientCert = "client.crt"
	pgbackrestTLSKeyClientKey  = "client.key"

	pgbackrestCAValidity     = 3650 * day
	pgbackrestCARenewBefore  = 365 * day
	pgbackrestLeafValidity   = 365 * day
	pgbackrestLeafRenewBefor = 60 * day
)

// pgbackrestTLSSecretName is the Secret with the pgBackRest TLS material.
func pgbackrestTLSSecretName(cluster *postgresv1alpha1.Cluster) string {
	return cluster.Name + "-pgbackrest-tls"
}

// pgbackrestServerDNSNames are the SANs of the TLS server certificate: the
// read-write Service, which the Jobs connect to.
func pgbackrestServerDNSNames(cluster *postgresv1alpha1.Cluster) []string {
	return []string{
		clusterHost(cluster),
		fmt.Sprintf("%s.%s.svc", cluster.Name, cluster.Namespace),
		fmt.Sprintf("%s.%s", cluster.Name, cluster.Namespace),
		cluster.Name,
	}
}

// leafCurrent reports whether leaf is signed by ca for usage (and, for a
// server certificate, names exactly dnsNames; for a client certificate has
// commonName), and is not due for renewal at now.
func leafCurrent(leaf, ca *keyPair, usage x509.ExtKeyUsage, cn string, dnsNames []string, now time.Time) bool {
	if leaf == nil || leaf.Cert.Subject.CommonName != cn || !slices.Equal(leaf.Cert.DNSNames, dnsNames) {
		return false
	}
	if !now.Before(leaf.Cert.NotAfter.Add(-pgbackrestLeafRenewBefor)) {
		return false
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	_, err := leaf.Cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{usage}})
	return err == nil
}

// planPgbackrestTLS returns the desired content of the pgBackRest TLS Secret
// from its current content (nil when it does not exist): valid material is
// kept, anything missing, invalid or due for renewal is regenerated (a new CA
// reissues both certificates). Also returns when to look again.
func planPgbackrestTLS(data map[string][]byte, dnsNames []string, now time.Time) (map[string][]byte, time.Time, error) {
	ca := parseKeyPair(data[TLSSecretKeyCA], data[pgbackrestTLSKeyCAKey])
	if ca == nil || !ca.Cert.IsCA || !now.Before(ca.Cert.NotAfter.Add(-pgbackrestCARenewBefore)) {
		var err error
		if ca, err = generateCA(commonName(dnsNames[0]+" pgBackRest CA"), now, pgbackrestCAValidity); err != nil {
			return nil, time.Time{}, fmt.Errorf("failed to generate the pgBackRest CA: %w", err)
		}
	}
	server := parseKeyPair(data[TLSSecretKeyCert], data[TLSSecretKeyKey])
	serverCN := commonName(dnsNames[0])
	if !leafCurrent(server, ca, x509.ExtKeyUsageServerAuth, serverCN, dnsNames, now) {
		var err error
		if server, err = issueLeafCert(ca, serverCN, dnsNames, x509.ExtKeyUsageServerAuth, now, pgbackrestLeafValidity); err != nil {
			return nil, time.Time{}, fmt.Errorf("failed to issue the pgBackRest server certificate: %w", err)
		}
	}
	clientCert := parseKeyPair(data[pgbackrestTLSKeyClientCert], data[pgbackrestTLSKeyClientKey])
	if !leafCurrent(clientCert, ca, x509.ExtKeyUsageClientAuth, pgbackrestClientCN, nil, now) {
		var err error
		if clientCert, err = issueLeafCert(ca, pgbackrestClientCN, nil, x509.ExtKeyUsageClientAuth, now, pgbackrestLeafValidity); err != nil {
			return nil, time.Time{}, fmt.Errorf("failed to issue the pgBackRest client certificate: %w", err)
		}
	}

	out := map[string][]byte{
		TLSSecretKeyCA:             ca.certPEM(),
		TLSSecretKeyCert:           server.certPEM(),
		pgbackrestTLSKeyClientCert: clientCert.certPEM(),
	}
	for key, kp := range map[string]*keyPair{
		pgbackrestTLSKeyCAKey:     ca,
		TLSSecretKeyKey:           server,
		pgbackrestTLSKeyClientKey: clientCert,
	} {
		pemKey, err := kp.keyPEM()
		if err != nil {
			return nil, time.Time{}, err
		}
		out[key] = pemKey
	}
	// Keep the stored key bytes when the key pair is unchanged (keyPEM
	// re-encodes and must not cause a Secret update on every reconcile).
	for _, pair := range [][2]string{
		{TLSSecretKeyCA, pgbackrestTLSKeyCAKey},
		{TLSSecretKeyCert, TLSSecretKeyKey},
		{pgbackrestTLSKeyClientCert, pgbackrestTLSKeyClientKey},
	} {
		if string(data[pair[0]]) == string(out[pair[0]]) && data[pair[1]] != nil {
			out[pair[1]] = data[pair[1]]
		}
	}

	next := ca.Cert.NotAfter.Add(-pgbackrestCARenewBefore)
	for _, leaf := range []*keyPair{server, clientCert} {
		if t := leaf.Cert.NotAfter.Add(-pgbackrestLeafRenewBefor); t.Before(next) {
			next = t
		}
	}
	return out, next, nil
}

// reconcilePgbackrestTLS creates or renews the pgBackRest TLS Secret and
// returns when it must be looked at again.
func (r *ClusterReconciler) reconcilePgbackrestTLS(ctx context.Context, cluster *postgresv1alpha1.Cluster) (time.Time, error) {
	s, err := r.getOwnedTLSSecret(ctx, cluster, pgbackrestTLSSecretName(cluster))
	if err != nil {
		return time.Time{}, err
	}
	data, next, err := planPgbackrestTLS(s.Data, pgbackrestServerDNSNames(cluster), r.now())
	if err != nil {
		return time.Time{}, err
	}
	return next, r.writeTLSSecret(ctx, cluster, s, data)
}

// cleanupPgbackrestTLS deletes the pgBackRest TLS Secret once the Cluster has
// no physical Backup. It runs after the StatefulSet stopped referencing it.
func (r *ClusterReconciler) cleanupPgbackrestTLS(ctx context.Context, cluster *postgresv1alpha1.Cluster) error {
	s := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: pgbackrestTLSSecretName(cluster), Namespace: cluster.Namespace}, s)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(s, cluster) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, s))
}
