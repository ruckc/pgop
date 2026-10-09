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
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

const (
	tlsVolumeName = "tls"
	hbaVolumeName = "hba"
	tlsMountPath  = "/etc/pgop/tls"
	hbaMountPath  = "/etc/pgop/hba"
	hbaFileName   = "pg_hba.conf"

	// tlsFileMode lets the postgres user read the private key through the pod
	// fsGroup (999) while the file stays root-owned: PostgreSQL accepts a
	// root-owned key with at most u=rw,g=r (0640).
	tlsFileMode int32 = 0o640
	hbaFileMode int32 = 0o644

	// sslRequestCode is the PostgreSQL SSLRequest message code (1234<<16 | 5679).
	sslRequestCode uint32 = 80877103

	tlsProbeTimeout = 10 * time.Second
)

// managedPgHBA is the pg_hba.conf used when spec.tls.requireTLS is true.
// Unix-socket connections (readiness/liveness probes and the password-sync
// postStart hook) keep trust auth, TCP connections must use TLS and SCRAM,
// and any non-TLS TCP connection is rejected.
const managedPgHBA = `# Managed by pgop (spec.tls.requireTLS). Changes are overwritten.
# TYPE    DATABASE USER ADDRESS    METHOD
local     all      all             trust
hostssl   all      all  0.0.0.0/0  scram-sha-256
hostssl   all      all  ::/0       scram-sha-256
hostnossl all      all  all        reject
`

// hbaConfigMapName is the name of the operator-managed pg_hba ConfigMap.
func hbaConfigMapName(cluster *postgresv1alpha1.Cluster) string {
	return cluster.Name + "-hba"
}

// clusterHost is the in-cluster DNS name of the Cluster's Service, which only
// ever routes to the primary. Everything that writes (the operator, Role and
// Database reconciles, backups, restores) and the standbys' replication
// connections use it. Server certificates must carry it as a SAN for
// verify-full to succeed.
func clusterHost(cluster *postgresv1alpha1.Cluster) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", cluster.Name, cluster.Namespace)
}

// clusterPort returns spec.port, defaulting to 5432.
func clusterPort(cluster *postgresv1alpha1.Cluster) int32 {
	if cluster.Spec.Port == 0 {
		return 5432
	}
	return cluster.Spec.Port
}

// tlsSecretName is the Secret holding the server certificate that is mounted
// into the pod: spec.tls.secretName, the Secret cert-manager writes for
// spec.tls.issuerRef, or the self-managed server Secret. spec.tls must be set.
func tlsSecretName(cluster *postgresv1alpha1.Cluster) string {
	t := cluster.Spec.TLS
	switch {
	case t.SecretName != "":
		return t.SecretName
	case t.IssuerRef != nil:
		return certManagerSecretName(cluster)
	default:
		return selfManagedServerSecretName(cluster)
	}
}

// tlsMaterial is the validated content of the server certificate Secret
// (see tlsSecretName).
type tlsMaterial struct {
	CAPEM []byte
	// Leaf is the server certificate (first certificate in tls.crt).
	Leaf *x509.Certificate
	// Hash identifies the certificate material (tls.crt + ca.crt); it is
	// recorded in status.tlsSecretHash once the server presents it.
	Hash string
}

// loadTLSMaterial fetches the Cluster's TLS Secret and validates it at now.
func loadTLSMaterial(ctx context.Context, c client.Reader, cluster *postgresv1alpha1.Cluster, now time.Time) (*tlsMaterial, error) {
	name := tlsSecretName(cluster)
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, secret); err != nil {
		return nil, fmt.Errorf("failed to get TLS Secret %q: %w", name, err)
	}
	m, err := parseTLSMaterial(secret.Data, clusterHost(cluster), now)
	if err != nil {
		return nil, fmt.Errorf("TLS Secret %q: %w", name, err)
	}
	return m, nil
}

// parseTLSMaterial validates a kubernetes.io/tls-style key set: all three
// keys are present, tls.crt/tls.key form a key pair, and the certificate
// chains to ca.crt, is currently valid, permits server authentication and
// names host. These are exactly the checks a verify-full client performs, so
// a Secret that passes here will not lock the operator out once mounted.
func parseTLSMaterial(data map[string][]byte, host string, now time.Time) (*tlsMaterial, error) {
	for _, k := range []string{TLSSecretKeyCert, TLSSecretKeyKey, TLSSecretKeyCA} {
		if len(data[k]) == 0 {
			return nil, fmt.Errorf("missing or empty key %q", k)
		}
	}
	certPEM, keyPEM, caPEM := data[TLSSecretKeyCert], data[TLSSecretKeyKey], data[TLSSecretKeyCA]

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("%s/%s are not a valid key pair: %w", TLSSecretKeyCert, TLSSecretKeyKey, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", TLSSecretKeyCert, err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%s contains no PEM certificates", TLSSecretKeyCA)
	}
	intermediates := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		if c, err := x509.ParseCertificate(der); err == nil {
			intermediates.AddCert(c)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName:       host,
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return nil, fmt.Errorf("certificate is not usable for %s with sslmode=verify-full: %w", host, err)
	}

	h := sha256.New()
	h.Write(certPEM)
	h.Write([]byte{0})
	h.Write(caPEM)
	return &tlsMaterial{
		CAPEM: caPEM,
		Leaf:  leaf,
		Hash:  hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// certVerifies reports whether the DER certificate der is valid for host
// under the CA bundle caPEM at now. It is used to decide whether a server that
// still presents an older certificate can be reached over verify-full.
func certVerifies(der, caPEM []byte, host string, now time.Time) bool {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return false
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return false
	}
	_, err = cert.Verify(x509.VerifyOptions{
		DNSName:     host,
		Roots:       roots,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return err == nil
}

// postgresTLSArgs returns the container args that enable TLS on the server,
// or nil when spec.tls is unset (the image default CMD is then used, as
// before). The official image entrypoint passes the "-c" options through to
// postgres, including to the temporary server it runs during initdb.
func postgresTLSArgs(spec *postgresv1alpha1.ClusterTLSSpec) []string {
	if spec == nil {
		return nil
	}
	args := []string{
		postgresBinary,
		"-c", "ssl=on",
		"-c", "ssl_cert_file=" + tlsMountPath + "/" + TLSSecretKeyCert,
		"-c", "ssl_key_file=" + tlsMountPath + "/" + TLSSecretKeyKey,
		"-c", "ssl_min_protocol_version=" + string(spec.GetMinProtocolVersion()),
	}
	if spec.IsRequireTLS() {
		args = append(args, "-c", hbaFileArg())
	}
	return args
}

// postgresTLSVolumes returns the pod volumes and container mounts for spec.tls
// (both nil when TLS is disabled). DefaultMode is always set explicitly so the
// API server's defaulting does not make the StatefulSet diff forever.
func postgresTLSVolumes(cluster *postgresv1alpha1.Cluster) ([]corev1.Volume, []corev1.VolumeMount) {
	spec := cluster.Spec.TLS
	if spec == nil {
		return nil, nil
	}
	volumes := []corev1.Volume{{
		Name: tlsVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  tlsSecretName(cluster),
				DefaultMode: new(tlsFileMode),
			},
		},
	}}
	mounts := []corev1.VolumeMount{tlsVolumeMount()}
	if spec.IsRequireTLS() {
		volumes = append(volumes, hbaVolume(cluster))
		mounts = append(mounts, hbaVolumeMount())
	}
	return volumes, mounts
}

// tlsVolumeMount mounts the server certificate Secret.
func tlsVolumeMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: tlsVolumeName, MountPath: tlsMountPath, ReadOnly: true}
}

// hbaVolume is the pod volume of the operator-managed pg_hba ConfigMap.
func hbaVolume(cluster *postgresv1alpha1.Cluster) corev1.Volume {
	return corev1.Volume{
		Name: hbaVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: hbaConfigMapName(cluster)},
				DefaultMode:          new(hbaFileMode),
			},
		},
	}
}

// hbaVolumeMount mounts the operator-managed pg_hba ConfigMap.
func hbaVolumeMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: hbaVolumeName, MountPath: hbaMountPath, ReadOnly: true}
}

// hbaFileArg is the server option that loads the operator-managed pg_hba.
func hbaFileArg() string { return "hba_file=" + hbaMountPath + "/" + hbaFileName }

// tlsRequired reports whether spec.tls.requireTLS is in effect.
func tlsRequired(cluster *postgresv1alpha1.Cluster) bool {
	return cluster.Spec.TLS != nil && cluster.Spec.TLS.IsRequireTLS()
}

// errServerTLSUnavailable means the server answered the SSLRequest with 'N'.
var errServerTLSUnavailable = errors.New("server does not accept TLS connections")

// probeServerCertificate connects to addr, negotiates TLS the way libpq does
// (SSLRequest, then a TLS handshake) and returns the DER of the certificate
// the server presents. The certificate is not verified here: the caller
// compares it byte-for-byte with the expected certificate, which it has
// already validated against the CA.
func probeServerCertificate(ctx context.Context, addr, serverName string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, tlsProbeTimeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	req := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 8), sslRequestCode)
	if _, err := conn.Write(req); err != nil {
		return nil, fmt.Errorf("failed to send SSLRequest: %w", err)
	}
	resp := make([]byte, 1)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return nil, fmt.Errorf("failed to read SSLRequest response: %w", err)
	}
	if resp[0] != 'S' {
		return nil, errServerTLSUnavailable
	}

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
		// Verification is replaced by an exact comparison with the expected
		// certificate (see above).
		InsecureSkipVerify: true, //nolint:gosec // compared byte-for-byte by the caller
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("TLS handshake failed: %w", err)
	}
	certs := tlsConn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, errors.New("server presented no certificate")
	}
	return certs[0].Raw, nil
}
