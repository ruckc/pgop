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
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

// Keys of the self-managed CA Secret ("<cluster>-ca"). tls.crt/tls.key hold
// the active (signing) CA, next.crt/next.key a CA prepared for the next
// rotation, and ca.crt the trust bundle clients must trust.
const (
	caSecretKeyNextCert = "next.crt"
	caSecretKeyNextKey  = "next.key"

	pemTypeCertificate = "CERTIFICATE"
)

// selfManagedCertPolicy sets the lifetimes of the self-managed CA and server
// certificate.
//
// A CA rotation is split in two steps so that clients never see a server
// certificate from a CA they do not trust yet:
//   - CAPrepareBefore the active CA expires, a next CA is generated and added
//     to the trust bundle (ca.crt) that clients receive through the credentials
//     Secrets, while server certificates are still signed by the active CA;
//   - CASwitchBefore the active CA expires, the next CA becomes the active one
//     and a new server certificate is issued from it. The previous CA stays in
//     the bundle until it expires, so the operator can still verify (and
//     reload) a server that presents the previous certificate.
type selfManagedCertPolicy struct {
	CAValidity      time.Duration
	CAPrepareBefore time.Duration
	CASwitchBefore  time.Duration

	ServerValidity    time.Duration
	ServerRenewBefore time.Duration
}

const day = 24 * time.Hour

// defaultSelfManagedCertPolicy: a 10 year CA, rotated in two steps three and
// one year before it expires, and a 90 day server certificate renewed 30
// days before it expires.
var defaultSelfManagedCertPolicy = selfManagedCertPolicy{
	CAValidity:        3650 * day,
	CAPrepareBefore:   3 * 365 * day,
	CASwitchBefore:    365 * day,
	ServerValidity:    90 * day,
	ServerRenewBefore: 30 * day,
}

// clockSkew backdates NotBefore so a freshly issued certificate is accepted
// by clients whose clock is slightly behind.
const clockSkew = 5 * time.Minute

// selfManagedCASecretName is the Secret holding the self-managed CA.
func selfManagedCASecretName(cluster *postgresv1alpha1.Cluster) string {
	return cluster.Name + "-ca"
}

// selfManagedServerSecretName is the Secret holding the self-managed server
// certificate; it is mounted into the pod.
func selfManagedServerSecretName(cluster *postgresv1alpha1.Cluster) string {
	return cluster.Name + "-server-cert"
}

// serverDNSNames are the SANs of operator-requested server certificates. The
// fully qualified Service name comes first: it is what verify-full checks.
func serverDNSNames(cluster *postgresv1alpha1.Cluster) []string {
	return []string{
		clusterHost(cluster),
		fmt.Sprintf("%s.%s.svc", cluster.Name, cluster.Namespace),
		fmt.Sprintf("%s.%s", cluster.Name, cluster.Namespace),
		cluster.Name,
	}
}

// keyPair is a parsed certificate and its private key.
type keyPair struct {
	Cert *x509.Certificate
	Key  crypto.Signer
}

func (kp *keyPair) certPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCertificate, Bytes: kp.Cert.Raw})
}

func (kp *keyPair) keyPEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(kp.Key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// parseKeyPair parses a PEM certificate and PKCS#8 private key; it returns
// nil when either is missing, malformed or they do not belong together.
func parseKeyPair(certPEM, keyPEM []byte) *keyPair {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil
	}
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil
	}
	pub, ok := cert.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(signer.Public()) {
		return nil
	}
	return &keyPair{Cert: cert, Key: signer}
}

// commonName truncates s to the 64 characters X.509 allows in a CommonName.
// Certificate verification uses the DNS SANs; the CommonName is informational.
func commonName(s string) string {
	if len(s) > 64 {
		return s[:64]
	}
	return s
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// generateCA creates a self-signed ECDSA P-256 CA valid for validity.
func generateCA(commonName string, now time.Time, validity time.Duration) (*keyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"pgop"}},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(validity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &keyPair{Cert: cert, Key: key}, nil
}

// issueServerCert issues an ECDSA P-256 server certificate for dnsNames from
// ca, valid until now+validity but never beyond the CA's own expiry.
func issueServerCert(ca *keyPair, dnsNames []string, now time.Time, validity time.Duration) (*keyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	notAfter := now.Add(validity)
	if notAfter.After(ca.Cert.NotAfter) {
		notAfter = ca.Cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName(dnsNames[0])},
		DNSNames:     dnsNames,
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &keyPair{Cert: cert, Key: key}, nil
}

// parseCertBundle returns the certificates in a PEM bundle, skipping blocks
// that do not parse.
func parseCertBundle(bundle []byte) []*x509.Certificate {
	var certs []*x509.Certificate
	for {
		var b *pem.Block
		b, bundle = pem.Decode(bundle)
		if b == nil {
			return certs
		}
		if b.Type != pemTypeCertificate {
			continue
		}
		if c, err := x509.ParseCertificate(b.Bytes); err == nil {
			certs = append(certs, c)
		}
	}
}

// selfManagedTLSPlan is the desired content of the self-managed Secrets.
type selfManagedTLSPlan struct {
	CA     map[string][]byte
	Server map[string][]byte
	// NextCheck is when the certificates must be looked at again (renewal,
	// CA preparation or switch, or pruning an expired CA from the bundle).
	NextCheck time.Time
}

// planSelfManagedTLS computes the self-managed CA and server Secret data from
// their current content (either may be nil). Valid content is kept as is;
// only what is missing, invalid or due for renewal is regenerated, so calling
// it repeatedly with its own output is a no-op until NextCheck.
func planSelfManagedTLS(caData, serverData map[string][]byte, dnsNames []string, now time.Time, p selfManagedCertPolicy) (*selfManagedTLSPlan, error) {
	if len(dnsNames) == 0 {
		return nil, errors.New("no DNS names for the server certificate")
	}
	active := parseKeyPair(caData[TLSSecretKeyCert], caData[TLSSecretKeyKey])
	next := parseKeyPair(caData[caSecretKeyNextCert], caData[caSecretKeyNextKey])
	if next != nil && (!next.Cert.IsCA || !now.Before(next.Cert.NotAfter)) {
		next = nil
	}

	var err error
	if active == nil || !active.Cert.IsCA || !now.Before(active.Cert.NotAfter.Add(-p.CASwitchBefore)) {
		// Missing, invalid or due for the switch: promote the prepared CA, or
		// (if there is none, e.g. the operator was not running when it was
		// due) generate a new one.
		if next != nil {
			active, next = next, nil
		} else if active, err = generateCA(commonName(dnsNames[0]+" CA"), now, p.CAValidity); err != nil {
			return nil, fmt.Errorf("failed to generate CA: %w", err)
		}
	}
	if next == nil && !now.Before(active.Cert.NotAfter.Add(-p.CAPrepareBefore)) {
		if next, err = generateCA(commonName(dnsNames[0]+" CA"), now, p.CAValidity); err != nil {
			return nil, fmt.Errorf("failed to generate CA: %w", err)
		}
	}

	// Trust bundle: active, next, then every other still valid CA from the
	// previous bundle (a CA that was switched away from stays trusted until it
	// expires).
	bundleCerts := []*x509.Certificate{active.Cert}
	if next != nil {
		bundleCerts = append(bundleCerts, next.Cert)
	}
	for _, c := range parseCertBundle(caData[TLSSecretKeyCA]) {
		if !c.IsCA || !now.Before(c.NotAfter) ||
			slices.ContainsFunc(bundleCerts, func(b *x509.Certificate) bool { return b.Equal(c) }) {
			continue
		}
		bundleCerts = append(bundleCerts, c)
	}
	var bundle bytes.Buffer
	for _, c := range bundleCerts {
		_ = pem.Encode(&bundle, &pem.Block{Type: pemTypeCertificate, Bytes: c.Raw})
	}

	activeKeyPEM, err := active.keyPEM()
	if err != nil {
		return nil, err
	}
	ca := map[string][]byte{
		TLSSecretKeyCert: active.certPEM(),
		TLSSecretKeyKey:  activeKeyPEM,
		TLSSecretKeyCA:   bundle.Bytes(),
	}
	if next != nil {
		nextKeyPEM, err := next.keyPEM()
		if err != nil {
			return nil, err
		}
		ca[caSecretKeyNextCert] = next.certPEM()
		ca[caSecretKeyNextKey] = nextKeyPEM
	}
	var server map[string][]byte
	leaf := parseKeyPair(serverData[TLSSecretKeyCert], serverData[TLSSecretKeyKey])
	if !serverCertCurrent(leaf, active, dnsNames, now, p) {
		if leaf, err = issueServerCert(active, dnsNames, now, p.ServerValidity); err != nil {
			return nil, fmt.Errorf("failed to issue server certificate: %w", err)
		}
		keyPEM, err := leaf.keyPEM()
		if err != nil {
			return nil, err
		}
		server = map[string][]byte{TLSSecretKeyCert: leaf.certPEM(), TLSSecretKeyKey: keyPEM}
	} else {
		server = maps.Clone(serverData)
	}
	server[TLSSecretKeyCA] = ca[TLSSecretKeyCA]

	nextCheck := leaf.Cert.NotAfter.Add(-p.ServerRenewBefore)
	earlier := func(t time.Time) {
		if t.Before(nextCheck) {
			nextCheck = t
		}
	}
	earlier(active.Cert.NotAfter.Add(-p.CASwitchBefore))
	if next == nil {
		earlier(active.Cert.NotAfter.Add(-p.CAPrepareBefore))
	}
	for _, c := range bundleCerts[1:] {
		earlier(c.NotAfter)
	}
	return &selfManagedTLSPlan{CA: ca, Server: server, NextCheck: nextCheck}, nil
}

// serverCertCurrent reports whether leaf can be kept: it is signed by the
// active CA, names exactly dnsNames, and is not yet due for renewal.
func serverCertCurrent(leaf, ca *keyPair, dnsNames []string, now time.Time, p selfManagedCertPolicy) bool {
	if leaf == nil || !slices.Equal(leaf.Cert.DNSNames, dnsNames) {
		return false
	}
	if !now.Before(leaf.Cert.NotAfter.Add(-p.ServerRenewBefore)) {
		return false
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	_, err := leaf.Cert.Verify(x509.VerifyOptions{
		DNSName:     dnsNames[0],
		Roots:       roots,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return err == nil
}

// reconcileSelfManagedTLS creates or renews the self-managed CA and server
// certificate Secrets. It returns when the certificates must be checked again.
func (r *ClusterReconciler) reconcileSelfManagedTLS(ctx context.Context, cluster *postgresv1alpha1.Cluster) (time.Time, error) {
	caSecret, err := r.getOwnedTLSSecret(ctx, cluster, selfManagedCASecretName(cluster))
	if err != nil {
		return time.Time{}, err
	}
	serverSecret, err := r.getOwnedTLSSecret(ctx, cluster, selfManagedServerSecretName(cluster))
	if err != nil {
		return time.Time{}, err
	}

	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	policy := defaultSelfManagedCertPolicy
	if r.SelfManagedCertPolicy != nil {
		policy = *r.SelfManagedCertPolicy
	}
	plan, err := planSelfManagedTLS(caSecret.Data, serverSecret.Data, serverDNSNames(cluster), now, policy)
	if err != nil {
		return time.Time{}, err
	}

	// The CA is written first: a server certificate is never persisted
	// without the CA that issued it.
	if err := r.writeTLSSecret(ctx, cluster, caSecret, plan.CA); err != nil {
		return time.Time{}, err
	}
	if err := r.writeTLSSecret(ctx, cluster, serverSecret, plan.Server); err != nil {
		return time.Time{}, err
	}
	return plan.NextCheck, nil
}

// errTLSSecretConflict means a Secret the operator wants to manage already
// exists and is not controlled by the Cluster.
var errTLSSecretConflict = errors.New("exists and is not owned by the Cluster; delete or rename it")

// getOwnedTLSSecret returns the named Secret if it is controlled by the
// Cluster, or an unsaved new Secret if it does not exist. A Secret owned by
// something else is never taken over.
func (r *ClusterReconciler) getOwnedTLSSecret(ctx context.Context, cluster *postgresv1alpha1.Cluster, name string) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, s)
	if apierrors.IsNotFound(err) {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: cluster.Namespace,
				Labels: map[string]string{
					LabelAppName:      AppNamePostgresql,
					LabelAppInstance:  cluster.Name,
					LabelAppManagedBy: LabelValuePgop,
				},
			},
			Type: corev1.SecretTypeTLS,
		}, nil
	}
	if err != nil {
		return nil, err
	}
	if !metav1.IsControlledBy(s, cluster) {
		return nil, &tlsNotReadyError{
			Reason:  ReasonInvalidTLSSecret,
			Message: fmt.Sprintf("Secret %q %s", name, errTLSSecretConflict),
		}
	}
	return s, nil
}

// writeTLSSecret creates s with data, or updates it when data changed.
func (r *ClusterReconciler) writeTLSSecret(ctx context.Context, cluster *postgresv1alpha1.Cluster, s *corev1.Secret, data map[string][]byte) error {
	if s.ResourceVersion == "" {
		s.Data = data
		if err := controllerutil.SetControllerReference(cluster, s, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, s)
	}
	if maps.EqualFunc(s.Data, data, bytes.Equal) {
		return nil
	}
	s.Data = data
	return r.Update(ctx, s)
}
