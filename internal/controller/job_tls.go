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
	corev1 "k8s.io/api/core/v1"
)

// libpq TLS settings for the pg_dump / pg_restore Jobs.
//
// The Jobs take their TLS settings from the Cluster credentials Secret when
// the pod starts, instead of having them written into the Job spec:
//
//   - PGSSLMODE comes from the Secret's "sslmode" key, which the Cluster
//     controller keeps in step with the server: "disable" without spec.tls,
//     "verify-full" while TLSReady is True, and "prefer" while TLS is enabled
//     but not confirmed yet (the same policy the operator uses for its own
//     connections, see clientTLSFor).
//   - The Secret's "ca.crt" (a copy of ca.crt from the server TLS Secret,
//     present only while TLS is active) is mounted at jobTLSCAPath, and
//     PGSSLROOTCERT points at it.
//
// PGHOST is the Service FQDN <cluster>.<ns>.svc.cluster.local, which is the
// name the server certificate is validated for, so verify-full checks the
// right host name.
//
// Because nothing TLS-specific is baked into the Job spec, turning TLS on or
// off, or rotating the CA, never requires the CronJob to be rewritten: the
// next Job pod picks up the current settings. With TLS off the CA file simply
// does not exist, which libpq ignores for sslmode=disable and prefer.
const (
	jobTLSVolumeName = "pg-ca"
	jobTLSMountPath  = "/etc/pgop/pg-ca"
	jobTLSCAPath     = jobTLSMountPath + "/" + SecretKeyCACert

	envPGSSLMode     = "PGSSLMODE"
	envPGSSLRootCert = "PGSSLROOTCERT"
)

// jobTLSEnv returns the libpq environment variables that make a Job connect
// with the sslmode and CA advertised in the credentials Secret.
func jobTLSEnv(credentialsSecret string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{
			Name: envPGSSLMode,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: credentialsSecret},
					Key:                  SecretKeySSLMode,
					// Secrets written before TLS support have no sslmode;
					// libpq then uses its default (prefer).
					Optional: new(true),
				},
			},
		},
		{Name: envPGSSLRootCert, Value: jobTLSCAPath},
	}
}

// jobTLSVolume mounts ca.crt from the credentials Secret. Both the Secret and
// the key are optional: ca.crt only exists while TLS is active.
func jobTLSVolume(credentialsSecret string) corev1.Volume {
	return corev1.Volume{
		Name: jobTLSVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  credentialsSecret,
				Items:       []corev1.KeyToPath{{Key: SecretKeyCACert, Path: SecretKeyCACert}},
				DefaultMode: new(int32(0o444)),
				Optional:    new(true),
			},
		},
	}
}

// jobTLSVolumeMount is the read-only mount of jobTLSVolume.
func jobTLSVolumeMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: jobTLSVolumeName, MountPath: jobTLSMountPath, ReadOnly: true}
}
