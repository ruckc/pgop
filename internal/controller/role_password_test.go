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
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

const (
	testCurrentPassword = "current-password"
	testRefPassword     = "from-the-ref"
	testPasswordKey     = "pass"
)

// Pure password selection and rotation scheduling (issue #25).
var _ = Describe("Role password selection", func() {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	newRole := func() *postgresv1alpha1.Role {
		return &postgresv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: "app", UID: "uid-1"}}
	}
	withRotation := func(role *postgresv1alpha1.Role, every time.Duration, rotatedAt *time.Time) *postgresv1alpha1.Role {
		role.Spec.PasswordRotation = &postgresv1alpha1.PasswordRotationSpec{Every: metav1.Duration{Duration: every}}
		if rotatedAt != nil {
			role.Status.PasswordRotatedAt = new(metav1.NewTime(*rotatedAt))
		}
		return role
	}

	It("uses the referenced password", func() {
		dp, err := choosePassword(newRole(), true, testRefPassword, testCurrentPassword, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(dp).To(Equal(desiredPassword{value: testRefPassword, fromRef: true}))
	})

	It("re-applies the referenced password on a rotate-password request", func() {
		role := newRole()
		role.Annotations = map[string]string{AnnotationRotatePassword: "1"}
		dp, err := choosePassword(role, true, testRefPassword, testCurrentPassword, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(dp).To(Equal(desiredPassword{value: testRefPassword, fromRef: true, force: true, rotationRequest: "1"}))
	})

	It("generates a password when there is none", func() {
		dp, err := choosePassword(newRole(), false, "", "", now)
		Expect(err).NotTo(HaveOccurred())
		Expect(dp.value).To(HaveLen(rolePasswordLength))
		Expect(dp.generated).To(BeTrue())
		Expect(dp.rotated).To(BeFalse())
	})

	It("keeps the current password without rotation", func() {
		dp, err := choosePassword(newRole(), false, "", testCurrentPassword, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(dp).To(Equal(desiredPassword{value: testCurrentPassword}))
	})

	It("keeps the current password and starts the schedule when no rotation time is known", func() {
		role := withRotation(newRole(), time.Hour, nil)
		dp, err := choosePassword(role, false, "", testCurrentPassword, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(dp).To(Equal(desiredPassword{value: testCurrentPassword}))
		recordPassword(role, dp, now)
		Expect(role.Status.PasswordRotatedAt.Time).To(Equal(now))
		Expect(nextRotationIn(role, now)).To(Equal(time.Hour))
	})

	It("keeps the current password until rotation is due and requeues for it", func() {
		rotatedAt := now.Add(-20 * time.Hour)
		role := withRotation(newRole(), 24*time.Hour, &rotatedAt)
		dp, err := choosePassword(role, false, "", testCurrentPassword, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(dp).To(Equal(desiredPassword{value: testCurrentPassword}))
		Expect(nextRotationIn(role, now)).To(Equal(4 * time.Hour))
	})

	It("rotates once the interval has elapsed", func() {
		rotatedAt := now.Add(-24 * time.Hour)
		role := withRotation(newRole(), 24*time.Hour, &rotatedAt)
		dp, err := choosePassword(role, false, "", testCurrentPassword, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(dp.value).NotTo(Equal(testCurrentPassword))
		Expect(dp.generated).To(BeTrue())
		Expect(dp.rotated).To(BeTrue())

		recordPassword(role, dp, now)
		Expect(role.Status.PasswordRotatedAt.Time).To(Equal(now))
		Expect(nextRotationIn(role, now)).To(Equal(24 * time.Hour))
	})

	It("never rotates more often than the minimum interval", func() {
		rotatedAt := now.Add(-30 * time.Minute)
		role := withRotation(newRole(), time.Minute, &rotatedAt)
		Expect(scheduledRotationDue(role, now)).To(BeFalse())
		Expect(nextRotationIn(role, now)).To(Equal(30 * time.Minute))
	})

	It("rotates once per new rotate-password annotation value", func() {
		role := newRole()
		role.Annotations = map[string]string{AnnotationRotatePassword: "incident-42"}
		dp, err := choosePassword(role, false, "", testCurrentPassword, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(dp.rotated).To(BeTrue())
		Expect(dp.rotationRequest).To(Equal("incident-42"))

		recordPassword(role, dp, now)
		Expect(role.Status.PasswordRotationRequest).To(Equal("incident-42"))
		dp, err = choosePassword(role, false, "", dp.value, now)
		Expect(err).NotTo(HaveOccurred())
		Expect(dp.rotated).To(BeFalse())
	})

	It("records the rotate-password request that came with the initial password", func() {
		role := newRole()
		role.Annotations = map[string]string{AnnotationRotatePassword: "x"}
		dp, err := choosePassword(role, false, "", "", now)
		Expect(err).NotTo(HaveOccurred())
		Expect(dp.rotated).To(BeFalse())
		recordPassword(role, dp, now)
		Expect(role.Status.PasswordRotationRequest).To(Equal("x"))
	})

	It("clears the rotation time while the password comes from a Secret", func() {
		rotatedAt := now.Add(-time.Hour)
		role := newRole()
		role.Status.PasswordRotatedAt = new(metav1.NewTime(rotatedAt))
		recordPassword(role, desiredPassword{value: testRefPassword, fromRef: true}, now)
		Expect(role.Status.PasswordRotatedAt).To(BeNil())
		Expect(nextRotationIn(role, now)).To(BeZero())
	})

	It("keeps no fingerprint in status and clears the deprecated passwordHash", func() {
		role := newRole()
		role.Status.PasswordHash = passwordFingerprint(role, testRefPassword)
		recordPassword(role, desiredPassword{value: testRefPassword, fromRef: true}, now)
		Expect(role.Status.PasswordHash).To(BeEmpty())
	})

	It("uses a salted fingerprint, not the password", func() {
		role := newRole()
		fp := passwordFingerprint(role, testRefPassword)
		Expect(fp).To(HaveLen(64))
		Expect(fp).NotTo(ContainSubstring(testRefPassword))
		other := newRole()
		other.UID = "uid-2"
		Expect(passwordFingerprint(other, testRefPassword)).NotTo(Equal(fp))
		Expect(passwordFingerprint(role, "different")).NotTo(Equal(fp))
	})

	It("knows the password is applied from the credentials Secret's fingerprint", func() {
		role := newRole()
		secret := func(annotations map[string]string) *corev1.Secret {
			return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}
		}
		fp := passwordFingerprint(role, testCurrentPassword)
		Expect(passwordApplied(role, nil, testCurrentPassword)).To(BeFalse())
		Expect(passwordApplied(role, secret(map[string]string{AnnotationPasswordFingerprint: fp}), testCurrentPassword)).To(BeTrue())
		Expect(passwordApplied(role, secret(map[string]string{AnnotationPasswordFingerprint: fp}), "other")).To(BeFalse())
		Expect(passwordApplied(role, secret(nil), testCurrentPassword)).To(BeFalse())

		By("falling back to the deprecated status.passwordHash for a Secret written before the annotation")
		role.Status.PasswordHash = fp
		Expect(passwordApplied(role, secret(nil), testCurrentPassword)).To(BeTrue())
		Expect(passwordApplied(role, secret(nil), "other")).To(BeFalse())
		By("trusting the annotation over status.passwordHash when both exist")
		Expect(passwordApplied(role, secret(map[string]string{AnnotationPasswordFingerprint: "x"}), testCurrentPassword)).To(BeFalse())
	})

	It("checks a password edited into the credentials Secret before setting it", func() {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds"}}
		hashed := "md5" + "0123456789abcdef0123456789abcdef"
		err := checkEditedPassword(desiredPassword{value: hashed}, s)
		ce, ok := errors.AsType[*conditionError](err)
		Expect(ok).To(BeTrue())
		Expect(ce.reason).To(Equal(ReasonPasswordSecretInvalid))
		Expect(err.Error()).NotTo(ContainSubstring(hashed))
		Expect(checkEditedPassword(desiredPassword{value: "nul\x00"}, s)).To(HaveOccurred())
		Expect(checkEditedPassword(desiredPassword{value: `it's \ fine`}, s)).To(Succeed())

		By("not checking a password that is already set, generated, from passwordSecretRef, or without a Secret")
		Expect(checkEditedPassword(desiredPassword{value: hashed, applied: true}, s)).To(Succeed())
		Expect(checkEditedPassword(desiredPassword{value: hashed, generated: true}, s)).To(Succeed())
		Expect(checkEditedPassword(desiredPassword{value: hashed, fromRef: true}, s)).To(Succeed())
		Expect(checkEditedPassword(desiredPassword{value: hashed}, nil)).To(Succeed())
	})
})

// missingSecretsClient is a client whose cache has no Secrets yet.
type missingSecretsClient struct{ client.Client }

func (c missingSecretsClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Secret); ok {
		return apierrors.NewNotFound(corev1.Resource("secrets"), key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

var _ = Describe("Role password Secrets", func() {
	const ns = "default"

	var (
		ctx    context.Context
		suffix string
	)

	BeforeEach(func() {
		ctx = context.Background()
		suffix = fmt.Sprintf("%d", time.Now().UnixNano())
	})

	cleanup := func(obj client.Object) {
		DeferCleanup(func() {
			obj.SetFinalizers(nil)
			_ = k8sClient.Update(ctx, obj)
			_ = k8sClient.Delete(ctx, obj)
		})
	}
	newRole := func(name string) *postgresv1alpha1.Role {
		return &postgresv1alpha1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       postgresv1alpha1.RoleSpec{ClusterRef: postgresv1alpha1.ClusterReference{Name: nonexistentCluster}},
		}
	}
	createSecret := func(name string, data map[string][]byte) *corev1.Secret {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: data}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		cleanup(s)
		return s
	}
	getSecret := func(name string) *corev1.Secret {
		s := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, s)).To(Succeed())
		return s
	}
	rr := func() *RoleReconciler { return &RoleReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()} }

	Context("API validation", func() {
		It("accepts a rotation interval of at least 1h", func() {
			role := newRole("rot-ok-" + suffix)
			role.Spec.PasswordRotation = &postgresv1alpha1.PasswordRotationSpec{Every: metav1.Duration{Duration: 720 * time.Hour}}
			Expect(k8sClient.Create(ctx, role)).To(Succeed())
			cleanup(role)
		})

		It("rejects a rotation interval below 1h", func() {
			role := newRole("rot-short-" + suffix)
			role.Spec.PasswordRotation = &postgresv1alpha1.PasswordRotationSpec{Every: metav1.Duration{Duration: 30 * time.Minute}}
			err := k8sClient.Create(ctx, role)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must be at least 1h"))
		})

		It("rejects passwordRotation together with passwordSecretRef", func() {
			role := newRole("rot-ref-" + suffix)
			role.Spec.PasswordRotation = &postgresv1alpha1.PasswordRotationSpec{Every: metav1.Duration{Duration: 24 * time.Hour}}
			role.Spec.PasswordSecretRef = &postgresv1alpha1.SecretKeySelector{Name: "pw", Key: "password"}
			err := k8sClient.Create(ctx, role)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("cannot be combined with passwordSecretRef"))
		})
	})

	Context("passwordSecretRef", func() {
		It("reads the referenced key and reports a missing Secret or key", func() {
			role := newRole("ref-" + suffix)
			role.Spec.PasswordSecretRef = &postgresv1alpha1.SecretKeySelector{Name: "pw-" + suffix, Key: testPasswordKey}

			_, err := rr().readPasswordSecretRef(ctx, role)
			ce, ok := errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue())
			Expect(ce.reason).To(Equal(ReasonPasswordSecretNotFound))

			createSecret("pw-"+suffix, map[string][]byte{"unrelated-key": []byte("x")})
			_, err = rr().readPasswordSecretRef(ctx, role)
			ce, ok = errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue())
			Expect(ce.reason).To(Equal(ReasonPasswordSecretNotFound))

			s := getSecret("pw-" + suffix)
			s.Data["pass"] = []byte(testRefPassword)
			Expect(k8sClient.Update(ctx, s)).To(Succeed())
			pw, err := rr().readPasswordSecretRef(ctx, role)
			Expect(err).NotTo(HaveOccurred())
			Expect(pw).To(Equal(testRefPassword))
		})

		It("rejects pre-hashed, non-UTF-8 and NUL-containing values without echoing them", func() {
			role := newRole("ref-invalid-" + suffix)
			role.Spec.PasswordSecretRef = &postgresv1alpha1.SecretKeySelector{Name: "pw-invalid-" + suffix, Key: testPasswordKey}
			createSecret("pw-invalid-"+suffix, map[string][]byte{testPasswordKey: []byte("placeholder")})

			for _, bad := range []string{
				"SCRAM-SHA-256$4096:c2FsdA==$c3RvcmVk:c2VydmVy",
				"md5" + "0123456789abcdef0123456789abcdef",
				"bad-\xff-utf8",
				"nul-\x00-byte",
			} {
				s := getSecret("pw-invalid-" + suffix)
				s.Data[testPasswordKey] = []byte(bad)
				Expect(k8sClient.Update(ctx, s)).To(Succeed())
				_, err := rr().readPasswordSecretRef(ctx, role)
				ce, ok := errors.AsType[*conditionError](err)
				Expect(ok).To(BeTrue(), "value %q", bad)
				Expect(ce.reason).To(Equal(ReasonPasswordSecretInvalid))
				Expect(err.Error()).NotTo(ContainSubstring(bad))
			}

			By("accepting quotes, backslashes and non-ASCII text")
			s := getSecret("pw-invalid-" + suffix)
			s.Data[testPasswordKey] = []byte(`it's a \ "päss"`)
			Expect(k8sClient.Update(ctx, s)).To(Succeed())
			pw, err := rr().readPasswordSecretRef(ctx, role)
			Expect(err).NotTo(HaveOccurred())
			Expect(pw).To(Equal(`it's a \ "päss"`))
		})

		It("refuses Secrets managed by pgop or used as a Cluster TLS Secret", func() {
			role := newRole("ref-protected-" + suffix)
			data := map[string][]byte{testPasswordKey: []byte(testRefPassword)}

			By("a Secret labeled as managed by pgop (such as <cluster>-credentials)")
			createSecret("pw-labeled-"+suffix, data)
			s := getSecret("pw-labeled-" + suffix)
			s.Labels = map[string]string{LabelAppManagedBy: LabelValuePgop}
			Expect(k8sClient.Update(ctx, s)).To(Succeed())
			role.Spec.PasswordSecretRef = &postgresv1alpha1.SecretKeySelector{Name: "pw-labeled-" + suffix, Key: testPasswordKey}
			_, err := rr().readPasswordSecretRef(ctx, role)
			ce, ok := errors.AsType[*conditionError](err)
			Expect(ok).To(BeTrue())
			Expect(ce.reason).To(Equal(ReasonRolePolicyViolation))
			Expect(err.Error()).To(ContainSubstring("managed by pgop"))

			By("a Secret owned by a pgop resource")
			createSecret("pw-owned-"+suffix, data)
			s = getSecret("pw-owned-" + suffix)
			s.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: postgresv1alpha1.GroupVersion.String(), Kind: "Role", Name: "owner-role", UID: "1234",
			}}
			Expect(k8sClient.Update(ctx, s)).To(Succeed())
			role.Spec.PasswordSecretRef.Name = "pw-owned-" + suffix
			_, err = rr().readPasswordSecretRef(ctx, role)
			Expect(err).To(MatchError(ContainSubstring(`owned by the Role "owner-role"`)))

			By("a Secret a Cluster uses as its TLS Secret")
			createSecret("pw-tls-"+suffix, data)
			cluster := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: "pw-tls-cluster-" + suffix, Namespace: role.Namespace},
				Spec: postgresv1alpha1.ClusterSpec{
					Image: DefaultPostgresImage,
					TLS:   &postgresv1alpha1.ClusterTLSSpec{SecretName: "pw-tls-" + suffix},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
			role.Spec.PasswordSecretRef.Name = "pw-tls-" + suffix
			_, err = rr().readPasswordSecretRef(ctx, role)
			Expect(err).To(MatchError(ContainSubstring("TLS Secret of Cluster")))
		})

		It("sets the Available reason to PasswordSecretNotFound", func() {
			role := newRole("ref-status-" + suffix)
			role.Spec.PasswordSecretRef = &postgresv1alpha1.SecretKeySelector{Name: "missing-" + suffix, Key: testPasswordKey}
			Expect(k8sClient.Create(ctx, role)).To(Succeed())
			cleanup(role)

			_, err := rr().readPasswordSecretRef(ctx, role)
			Expect(err).To(HaveOccurred())
			res, uerr := rr().updateStatus(ctx, role, "", err)
			Expect(uerr).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			cond := meta.FindStatusCondition(role.Status.Conditions, ConditionTypeAvailable)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(ReasonPasswordSecretNotFound))
		})

		It("maps a referenced Secret to the Roles using it", func() {
			role := newRole("ref-map-" + suffix)
			role.Spec.PasswordSecretRef = &postgresv1alpha1.SecretKeySelector{Name: "app-pw-" + suffix, Key: testPasswordKey}
			Expect(k8sClient.Create(ctx, role)).To(Succeed())
			cleanup(role)
			plain := newRole("ref-plain-" + suffix)
			Expect(k8sClient.Create(ctx, plain)).To(Succeed())
			cleanup(plain)

			reqs := rr().rolesForPasswordSecret(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "app-pw-" + suffix, Namespace: ns}})
			Expect(reqs).To(ConsistOf(reconcileRequest(role)))
			Expect(rr().rolesForPasswordSecret(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "unrelated-" + suffix, Namespace: ns}})).To(BeEmpty())
		})
	})

	Context("rotation with a stale cached Role", func() {
		It("re-reads the Role and does not rotate twice", func() {
			role := newRole("stale-" + suffix)
			role.Annotations = map[string]string{AnnotationRotatePassword: "req-1"}
			Expect(k8sClient.Create(ctx, role)).To(Succeed())
			cleanup(role)
			stale := role.DeepCopy()

			By("recording on the server that the request was handled")
			role.Status.PasswordRotationRequest = "req-1"
			Expect(k8sClient.Status().Update(ctx, role)).To(Succeed())

			existing := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
					AnnotationPasswordFingerprint: passwordFingerprint(role, testCurrentPassword)}},
				Data: map[string][]byte{SecretKeyPassword: []byte(testCurrentPassword)},
			}

			withoutReader := stale.DeepCopy()
			dp, _, err := rr().resolvePassword(ctx, withoutReader, existing, time.Now())
			Expect(err).NotTo(HaveOccurred())
			Expect(dp.rotated).To(BeTrue(), "the stale Role alone asks for a rotation")

			r := rr()
			r.APIReader = k8sClient
			dp, got, err := r.resolvePassword(ctx, stale, existing, time.Now())
			Expect(err).NotTo(HaveOccurred())
			Expect(dp).To(Equal(desiredPassword{value: testCurrentPassword, applied: true}))
			Expect(got).To(BeIdenticalTo(existing))
			Expect(stale.Status.PasswordRotationRequest).To(Equal("req-1"))
			Expect(stale.ResourceVersion).To(Equal(role.ResourceVersion))
		})
	})

	Context("password from a stale cached credentials Secret", func() {
		It("re-reads the Secret and never sends a password older than the one PostgreSQL has", func() {
			role := newRole("stale-secret-" + suffix)
			Expect(k8sClient.Create(ctx, role)).To(Succeed())
			cleanup(role)

			By("the server already holding the rotated password, as written by the previous reconcile")
			const rotated = "rotated-password"
			current := createSecret("stale-secret-creds-"+suffix, map[string][]byte{SecretKeyPassword: []byte(rotated)})
			current.Annotations = map[string]string{AnnotationPasswordFingerprint: passwordFingerprint(role, rotated)}
			Expect(k8sClient.Update(ctx, current)).To(Succeed())

			By("the cache still holding the Secret from before the rotation (written before the fingerprint annotation existed)")
			stale := current.DeepCopy()
			stale.ResourceVersion = "1"
			stale.Annotations = nil
			stale.Data = map[string][]byte{SecretKeyPassword: []byte(testCurrentPassword)}

			dp, _, err := rr().resolvePassword(ctx, role.DeepCopy(), stale, time.Now())
			Expect(err).NotTo(HaveOccurred())
			Expect(dp.value).To(Equal(testCurrentPassword))
			Expect(dp.applied).To(BeFalse(), "without a fresh read the stale password would be sent, rolling the rotation back")

			r := rr()
			r.APIReader = k8sClient
			dp, got, err := r.resolvePassword(ctx, role, stale, time.Now())
			Expect(err).NotTo(HaveOccurred())
			Expect(dp).To(Equal(desiredPassword{value: rotated, applied: true}))
			Expect(got.ResourceVersion).To(Equal(current.ResourceVersion))
			Expect(string(got.Data[SecretKeyPassword])).To(Equal(rotated))

			By("still sending a password that is new according to the fresh Secret")
			current = getSecret(current.Name)
			current.Data[SecretKeyPassword] = []byte("edited-by-hand")
			Expect(k8sClient.Update(ctx, current)).To(Succeed())
			dp, _, err = r.resolvePassword(ctx, role, stale, time.Now())
			Expect(err).NotTo(HaveOccurred())
			Expect(dp).To(Equal(desiredPassword{value: "edited-by-hand"}))
		})
	})

	Context("credentials Secret written from a stale cache", func() {
		var (
			cluster *postgresv1alpha1.Cluster
			role    *postgresv1alpha1.Role
		)
		BeforeEach(func() {
			cluster = &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: "stale-cl-" + suffix, Namespace: ns},
				Spec:       postgresv1alpha1.ClusterSpec{Image: DefaultPostgresImage},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			cleanup(cluster)
			role = newRole("stale-w-" + suffix)
			role.Spec.ClusterRef.Name = cluster.Name
			Expect(k8sClient.Create(ctx, role)).To(Succeed())
			cleanup(role)
		})

		It("does not overwrite a rotated password with the stale one on a conflict", func() {
			By("a Secret the cache still shows with the old password")
			const rotated = "rotated-password"
			stale := createSecret(credentialsSecretName(role, cluster), map[string][]byte{
				SecretKeyPassword: []byte(testCurrentPassword)})
			stale.Annotations = map[string]string{AnnotationPasswordFingerprint: passwordFingerprint(role, testCurrentPassword)}
			Expect(k8sClient.Update(ctx, stale)).To(Succeed())
			stale = stale.DeepCopy()

			By("the previous reconcile having rotated it on the server")
			current := getSecret(stale.Name)
			current.Data[SecretKeyPassword] = []byte(rotated)
			current.Annotations[AnnotationPasswordFingerprint] = passwordFingerprint(role, rotated)
			Expect(k8sClient.Update(ctx, current)).To(Succeed())

			By("converging the stale Secret (its labels are missing, so it is updated and conflicts)")
			r := rr()
			r.APIReader = k8sClient
			_, err := r.reconcileCredentialsSecret(ctx, role, cluster, stale, testCurrentPassword, false)
			Expect(err).To(MatchError(errCredentialsSecretChanged))
			s := getSecret(stale.Name)
			Expect(string(s.Data[SecretKeyPassword])).To(Equal(rotated))
			Expect(s.Annotations).To(HaveKeyWithValue(AnnotationPasswordFingerprint, passwordFingerprint(role, rotated)))

			By("still writing a password this reconcile set in PostgreSQL")
			_, err = r.reconcileCredentialsSecret(ctx, role, cluster, stale, "newer-password", true)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(getSecret(stale.Name).Data[SecretKeyPassword])).To(Equal("newer-password"))
		})

		It("looks a Secret missing from the cache up uncached before generating a password", func() {
			createSecret(credentialsSecretName(role, cluster), map[string][]byte{SecretKeyPassword: []byte(testCurrentPassword)})

			r := &RoleReconciler{Client: missingSecretsClient{k8sClient}, Scheme: k8sClient.Scheme()}
			s, err := r.getCredentialsSecret(ctx, role, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(s).To(BeNil(), "the cache alone reports the Secret missing")

			r.APIReader = k8sClient
			s, err = r.getCredentialsSecret(ctx, role, cluster)
			Expect(err).NotTo(HaveOccurred())
			Expect(s).NotTo(BeNil())
			Expect(string(s.Data[SecretKeyPassword])).To(Equal(testCurrentPassword))
		})
	})

	Context("credentials Secrets", func() {
		It("updates the password and uri of the role Secret and the Database Secret follows", func() {
			cluster := &postgresv1alpha1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: "pw-cluster-" + suffix, Namespace: ns},
				Spec:       postgresv1alpha1.ClusterSpec{Image: DefaultPostgresImage},
			}
			Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
			cleanup(cluster)
			role := newRole("pw-owner-" + suffix)
			role.Spec.ClusterRef.Name = cluster.Name
			Expect(k8sClient.Create(ctx, role)).To(Succeed())
			cleanup(role)

			secretName, err := rr().reconcileCredentialsSecret(ctx, role, cluster, nil, "old-password", true)
			Expect(err).NotTo(HaveOccurred())
			s := getSecret(secretName)
			Expect(isRoleCredentialsSecret(s)).To(BeTrue())
			Expect(string(s.Data[SecretKeyURI])).To(ContainSubstring(":old-password@"))
			Expect(s.Annotations).To(HaveKeyWithValue(AnnotationPasswordFingerprint, passwordFingerprint(role, "old-password")))

			db := &postgresv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{Name: "pw-db-" + suffix, Namespace: ns},
				Spec: postgresv1alpha1.DatabaseSpec{
					ClusterRef: postgresv1alpha1.ClusterReference{Name: cluster.Name}, Owner: role.Name},
			}
			Expect(k8sClient.Create(ctx, db)).To(Succeed())
			cleanup(db)
			otherCluster := db.DeepCopy()
			otherCluster.ObjectMeta = metav1.ObjectMeta{Name: "pw-db-other-" + suffix, Namespace: ns}
			otherCluster.Spec.ClusterRef.Name = nonexistentCluster
			Expect(k8sClient.Create(ctx, otherCluster)).To(Succeed())
			cleanup(otherCluster)

			dr := &DatabaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(dr.reconcileCredentialsSecret(ctx, db, role, cluster)).To(Succeed())
			dbSecretName := db.Name + "-" + role.Name + "-credentials"
			Expect(string(getSecret(dbSecretName).Data[SecretKeyPassword])).To(Equal("old-password"))

			By("changing the password")
			_, err = rr().reconcileCredentialsSecret(ctx, role, cluster, s, "new-password", true)
			Expect(err).NotTo(HaveOccurred())
			s = getSecret(secretName)
			Expect(string(s.Data[SecretKeyPassword])).To(Equal("new-password"))
			Expect(string(s.Data[SecretKeyUsername])).To(Equal(role.Name))
			Expect(string(s.Data[SecretKeyURI])).To(ContainSubstring(":new-password@"))
			Expect(string(s.Data[SecretKeyURI])).NotTo(ContainSubstring("old-password"))
			Expect(s.Annotations).To(HaveKeyWithValue(AnnotationPasswordFingerprint, passwordFingerprint(role, "new-password")))

			By("mapping the role Secret to the Databases it owns on the same Cluster")
			Expect(dr.databasesForRoleSecret(ctx, s)).To(ConsistOf(reconcileRequest(db)))
			Expect(dr.databasesForRoleSecret(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: ns}})).To(BeEmpty())

			Expect(dr.reconcileCredentialsSecret(ctx, db, role, cluster)).To(Succeed())
			dbSecret := getSecret(dbSecretName)
			Expect(string(dbSecret.Data[SecretKeyPassword])).To(Equal("new-password"))
			Expect(string(dbSecret.Data[SecretKeyURI])).To(ContainSubstring(":new-password@"))
		})

		It("restores the labels of a role Secret that lost them", func() {
			cluster := &postgresv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "lbl-" + suffix}}
			role := newRole("lbl-role-" + suffix)
			s := &corev1.Secret{Data: map[string][]byte{SecretKeyPassword: []byte("p")}}
			Expect(convergeRoleSecret(s, role, "p", "h", 5432, clientTLS{SSLMode: postgres.SSLModeDisable})).To(BeTrue())
			Expect(isRoleCredentialsSecret(s)).To(BeTrue())
			Expect(s.Annotations).To(HaveKeyWithValue(AnnotationPasswordFingerprint, passwordFingerprint(role, "p")))
			Expect(convergeRoleSecret(s, role, "p", "h", 5432, clientTLS{SSLMode: postgres.SSLModeDisable})).To(BeFalse())
			Expect(credentialsSecretName(role, cluster)).To(Equal("lbl-" + suffix + "-" + role.Name + "-credentials"))
		})
	})
})
