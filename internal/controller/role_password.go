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
	"encoding/hex"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

const (
	// AnnotationRotatePassword requests an immediate password rotation for a
	// Role when it is set to a value the operator has not acted on yet (see
	// status.passwordRotationRequest). For a Role using passwordSecretRef it
	// re-applies the referenced password instead.
	AnnotationRotatePassword = "pgop.ruck.io/rotate-password"

	// ReasonPasswordSecretNotFound: spec.passwordSecretRef names a Secret or
	// key that does not exist (or holds an empty value).
	ReasonPasswordSecretNotFound = "PasswordSecretNotFound"
	// ReasonPasswordRotated is the Event reason for a password rotation.
	ReasonPasswordRotated = "PasswordRotated"

	// minPasswordRotationInterval is the shortest rotation interval honored,
	// matching the CRD validation.
	minPasswordRotationInterval = time.Hour

	// rolePasswordLength is the length of generated role passwords.
	rolePasswordLength = 32
)

// conditionError is an error that carries the Available condition reason to
// report for it.
type conditionError struct {
	reason string
	err    error
}

func (e *conditionError) Error() string { return e.err.Error() }
func (e *conditionError) Unwrap() error { return e.err }

// desiredPassword is the password a Role should have after this reconcile.
// value is never logged or written to status.
type desiredPassword struct {
	value string
	// generated is true when value was generated in this reconcile (no
	// password yet, or a rotation).
	generated bool
	// rotated is true when a generated value replaces an existing password.
	rotated bool
	// fromRef is true when value comes from spec.passwordSecretRef.
	fromRef bool
	// force sends the password to PostgreSQL even when its fingerprint
	// matches status.passwordHash (a rotate-password request on a Role that
	// uses passwordSecretRef).
	force bool
	// rotationRequest is the rotate-password annotation value acted on, or ""
	// when the annotation holds no new request.
	rotationRequest string
}

// passwordFingerprint returns the salted SHA-256 fingerprint of password that
// is recorded in status.passwordHash. The Role's UID is the salt, so equal
// passwords of different Roles do not share a fingerprint.
func passwordFingerprint(role *postgresv1alpha1.Role, password string) string {
	sum := sha256.Sum256([]byte(string(role.UID) + "\x00" + password))
	return hex.EncodeToString(sum[:])
}

// rotationInterval returns the effective scheduled rotation interval, or 0
// when scheduled rotation is off.
func rotationInterval(role *postgresv1alpha1.Role) time.Duration {
	if role.Spec.PasswordRotation == nil {
		return 0
	}
	return max(role.Spec.PasswordRotation.Every.Duration, minPasswordRotationInterval)
}

// pendingRotationRequest returns the rotate-password annotation value when
// it has not been acted on yet, otherwise "".
func pendingRotationRequest(role *postgresv1alpha1.Role) string {
	req := role.Annotations[AnnotationRotatePassword]
	if req == "" || req == role.Status.PasswordRotationRequest {
		return ""
	}
	return req
}

// scheduledRotationDue reports whether the scheduled rotation of an
// operator-generated password is due at now. Without a recorded
// passwordRotatedAt the schedule has no base yet and nothing is due; the
// reconcile then starts the schedule at now.
func scheduledRotationDue(role *postgresv1alpha1.Role, now time.Time) bool {
	every := rotationInterval(role)
	if every == 0 || role.Status.PasswordRotatedAt == nil {
		return false
	}
	return !now.Before(role.Status.PasswordRotatedAt.Add(every))
}

// nextRotationIn returns how long until the next scheduled rotation, based
// on the (already updated) status, or 0 when scheduled rotation is off.
func nextRotationIn(role *postgresv1alpha1.Role, now time.Time) time.Duration {
	every := rotationInterval(role)
	if every == 0 || role.Status.PasswordRotatedAt == nil {
		return 0
	}
	return max(role.Status.PasswordRotatedAt.Add(every).Sub(now), time.Second)
}

// choosePassword decides the Role's password from the referenced password
// (refPassword, only used when fromRef), the password currently in the
// credentials Secret (current, "" when there is none) and the rotation
// settings. It is pure except for generating random passwords.
func choosePassword(role *postgresv1alpha1.Role, fromRef bool, refPassword, current string, now time.Time) (desiredPassword, error) {
	req := pendingRotationRequest(role)
	if fromRef {
		return desiredPassword{value: refPassword, fromRef: true, force: req != "", rotationRequest: req}, nil
	}
	if current != "" && req == "" && !scheduledRotationDue(role, now) {
		return desiredPassword{value: current}, nil
	}
	pw, err := generateRolePassword(rolePasswordLength)
	if err != nil {
		return desiredPassword{}, fmt.Errorf("failed to generate password: %w", err)
	}
	return desiredPassword{value: pw, generated: true, rotated: current != "", rotationRequest: req}, nil
}

// readPasswordSecretRef reads the password named by spec.passwordSecretRef.
// A missing Secret, missing key or empty value is a conditionError with
// reason PasswordSecretNotFound; the operator never falls back to a
// generated password.
func (r *RoleReconciler) readPasswordSecretRef(ctx context.Context, role *postgresv1alpha1.Role) (string, error) {
	ref := role.Spec.PasswordSecretRef
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: role.Namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", &conditionError{reason: ReasonPasswordSecretNotFound,
				err: fmt.Errorf("password Secret %q not found", ref.Name)}
		}
		return "", fmt.Errorf("failed to get password Secret %q: %w", ref.Name, err)
	}
	pw := secret.Data[ref.Key]
	if len(pw) == 0 {
		return "", &conditionError{reason: ReasonPasswordSecretNotFound,
			err: fmt.Errorf("password Secret %q has no (or an empty) key %q", ref.Name, ref.Key)}
	}
	return string(pw), nil
}

// resolvePassword returns the password the Role should have. existing is the
// role credentials Secret, or nil when it does not exist yet.
func (r *RoleReconciler) resolvePassword(ctx context.Context, role *postgresv1alpha1.Role, existing *corev1.Secret, now time.Time) (desiredPassword, error) {
	var refPassword string
	fromRef := role.Spec.PasswordSecretRef != nil
	if fromRef {
		var err error
		if refPassword, err = r.readPasswordSecretRef(ctx, role); err != nil {
			return desiredPassword{}, err
		}
	}
	var current string
	if existing != nil {
		current = string(existing.Data[SecretKeyPassword])
	}
	dp, err := choosePassword(role, fromRef, refPassword, current, now)
	if err != nil || !dp.rotated || r.APIReader == nil {
		return dp, err
	}
	// A rotation is about to replace a working password. The cached Role may
	// lag behind the status written by the reconcile that just rotated (whose
	// Secret update triggered this one), so confirm with a fresh read that
	// the rotation is still due before rotating again.
	fresh := &postgresv1alpha1.Role{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(role), fresh); err != nil {
		return desiredPassword{}, fmt.Errorf("failed to re-read Role before rotating its password: %w", err)
	}
	if fresh.ResourceVersion == role.ResourceVersion {
		return dp, nil
	}
	*role = *fresh
	return choosePassword(role, fromRef, refPassword, current, now)
}

// recordPassword updates the password fields of the Role status once the
// password is set in PostgreSQL and in the credentials Secret.
func recordPassword(role *postgresv1alpha1.Role, dp desiredPassword, now time.Time) {
	role.Status.PasswordHash = passwordFingerprint(role, dp.value)
	switch {
	case dp.fromRef:
		role.Status.PasswordRotatedAt = nil
	case dp.generated:
		role.Status.PasswordRotatedAt = new(metav1.NewTime(now))
	case role.Status.PasswordRotatedAt == nil && rotationInterval(role) > 0:
		// Rotation was enabled for a password generated before the
		// operator tracked generation times: start the schedule now.
		role.Status.PasswordRotatedAt = new(metav1.NewTime(now))
	}
	if dp.rotationRequest != "" {
		role.Status.PasswordRotationRequest = dp.rotationRequest
	}
}

// rolesForPasswordSecret maps a Secret to the Roles in its namespace whose
// spec.passwordSecretRef names it, so a changed password is applied.
func (r *RoleReconciler) rolesForPasswordSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	roles := &postgresv1alpha1.RoleList{}
	if err := r.List(ctx, roles, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Roles for password Secret", "secret", obj.GetName())
		return nil
	}
	var requests []reconcile.Request
	for i := range roles.Items {
		if ref := roles.Items[i].Spec.PasswordSecretRef; ref != nil && ref.Name == obj.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&roles.Items[i])})
		}
	}
	return requests
}
