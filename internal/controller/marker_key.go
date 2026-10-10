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
	"crypto/rand"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

// markerKeySize is the size of the per-Cluster ownership-marker key.
const markerKeySize = 32

// secretKeyMarkerKey is the data key of the marker key Secret.
const secretKeyMarkerKey = "key"

// markerKeySecretName is the Secret holding the Cluster's ownership-marker
// key (see markerSigner). It is created by the Cluster controller, owned by
// the Cluster, labeled as managed by pgop (so passwordSecretRef cannot read
// it) and never mounted into any pod. If it is lost, a new key is generated
// and existing markers stop matching: Roles and Databases then report
// RoleNotManaged / DatabaseNotManaged until a superuser clears the comments
// (COMMENT ON ... IS NULL), after which the objects recorded in their
// resources' status are marked again with the new key.
func markerKeySecretName(cluster *postgresv1alpha1.Cluster) string {
	return cluster.Name + "-marker-key"
}

// ensureMarkerKeySecret creates the Cluster's marker key Secret when it does
// not exist. An existing Secret is never changed.
func ensureMarkerKeySecret(ctx context.Context, c client.Client, scheme *runtime.Scheme, cluster *postgresv1alpha1.Cluster) error {
	secret := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Name: markerKeySecretName(cluster), Namespace: cluster.Namespace}, secret)
	if err == nil || !apierrors.IsNotFound(err) {
		return err
	}
	key := make([]byte, markerKeySize)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("failed to generate the marker key: %w", err)
	}
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      markerKeySecretName(cluster),
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				LabelAppName:      AppNamePostgresql,
				LabelAppInstance:  cluster.Name,
				LabelAppManagedBy: LabelValuePgop,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{secretKeyMarkerKey: key},
	}
	if err := controllerutil.SetControllerReference(cluster, secret, scheme); err != nil {
		return err
	}
	if err := c.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// loadMarkerSigner returns the Cluster's marker signer, reading its key
// Secret. It fails while the Secret does not exist yet (the Cluster
// controller creates it) or holds no usable key.
func loadMarkerSigner(ctx context.Context, c client.Reader, cluster *postgresv1alpha1.Cluster) (markerSigner, error) {
	secret := &corev1.Secret{}
	name := markerKeySecretName(cluster)
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, secret); err != nil {
		return markerSigner{}, fmt.Errorf("failed to get the ownership-marker key Secret %q: %w", name, err)
	}
	key := secret.Data[secretKeyMarkerKey]
	if len(key) < markerKeySize {
		return markerSigner{}, fmt.Errorf("the ownership-marker key Secret %q has no usable %q key", name, secretKeyMarkerKey)
	}
	return newMarkerSigner(cluster, key), nil
}
