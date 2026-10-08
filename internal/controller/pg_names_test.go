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
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

const testPGDatabaseName = "rs_app_db"

func reconcileRequest(obj client.Object) reconcile.Request {
	return reconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)}
}

// Tests for the optional spec.roleName / spec.databaseName overrides (issue #19).
var _ = Describe("PostgreSQL name overrides", func() {
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

	newRole := func(name, pgName string) *postgresv1alpha1.Role {
		return &postgresv1alpha1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: postgresv1alpha1.RoleSpec{
				ClusterRef: postgresv1alpha1.ClusterReference{Name: nonexistentCluster},
				RoleName:   pgName,
			},
		}
	}

	newDatabase := func(name, pgName, owner string) *postgresv1alpha1.Database {
		return &postgresv1alpha1.Database{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: postgresv1alpha1.DatabaseSpec{
				ClusterRef:   postgresv1alpha1.ClusterReference{Name: nonexistentCluster},
				DatabaseName: pgName,
				Owner:        owner,
			},
		}
	}

	newCluster := func(name string) *postgresv1alpha1.Cluster {
		cluster := &postgresv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       postgresv1alpha1.ClusterSpec{Image: DefaultPostgresImage},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		cleanup(cluster)
		return cluster
	}

	invalidNames := []string{"RS_App", "rs-app", "$x", "1abc", strings.Repeat("a", 64)}

	Context("Role spec.roleName validation", func() {
		It("accepts a lowercase identifier with underscores", func() {
			role := newRole("rs-app-"+suffix, "rs_app")
			Expect(k8sClient.Create(ctx, role)).To(Succeed())
			cleanup(role)
			Expect(role.PostgresName()).To(Equal("rs_app"))
		})

		It("accepts a 63 character name", func() {
			role := newRole("long-"+suffix, strings.Repeat("a", 63))
			Expect(k8sClient.Create(ctx, role)).To(Succeed())
			cleanup(role)
		})

		for _, name := range append(invalidNames, "pg_foo", "postgres", "pgop_operator") {
			It(fmt.Sprintf("rejects %q", name), func() {
				err := k8sClient.Create(ctx, newRole("bad-"+suffix, name))
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got %v", err)
			})
		}

		It("rejects changing, adding or removing roleName", func() {
			withName := newRole("imm-a-"+suffix, "rs_app")
			Expect(k8sClient.Create(ctx, withName)).To(Succeed())
			cleanup(withName)

			changed := withName.DeepCopy()
			changed.Spec.RoleName = "rs_other"
			Expect(apierrors.IsInvalid(k8sClient.Update(ctx, changed))).To(BeTrue())

			removed := withName.DeepCopy()
			removed.Spec.RoleName = ""
			Expect(apierrors.IsInvalid(k8sClient.Update(ctx, removed))).To(BeTrue())

			withoutName := newRole("imm-b-"+suffix, "")
			Expect(k8sClient.Create(ctx, withoutName)).To(Succeed())
			cleanup(withoutName)

			added := withoutName.DeepCopy()
			added.Spec.RoleName = "rs_app"
			Expect(apierrors.IsInvalid(k8sClient.Update(ctx, added))).To(BeTrue())

			By("allowing unrelated spec updates")
			other := withName.DeepCopy()
			other.Spec.CreateDB = true
			Expect(k8sClient.Update(ctx, other)).To(Succeed())
		})
	})

	Context("Database spec.databaseName validation", func() {
		It("accepts a lowercase identifier with underscores", func() {
			db := newDatabase("rs-app-db-"+suffix, testPGDatabaseName, "")
			Expect(k8sClient.Create(ctx, db)).To(Succeed())
			cleanup(db)
			Expect(db.PostgresName()).To(Equal(testPGDatabaseName))
		})

		for _, name := range append(invalidNames, "postgres", "template0", "template1") {
			It(fmt.Sprintf("rejects %q", name), func() {
				err := k8sClient.Create(ctx, newDatabase("bad-"+suffix, name, ""))
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got %v", err)
			})
		}

		It("rejects changing, adding or removing databaseName", func() {
			withName := newDatabase("imm-a-"+suffix, testPGDatabaseName, "")
			Expect(k8sClient.Create(ctx, withName)).To(Succeed())
			cleanup(withName)

			changed := withName.DeepCopy()
			changed.Spec.DatabaseName = "rs_other_db"
			Expect(apierrors.IsInvalid(k8sClient.Update(ctx, changed))).To(BeTrue())

			removed := withName.DeepCopy()
			removed.Spec.DatabaseName = ""
			Expect(apierrors.IsInvalid(k8sClient.Update(ctx, removed))).To(BeTrue())

			withoutName := newDatabase("imm-b-"+suffix, "", "")
			Expect(k8sClient.Create(ctx, withoutName)).To(Succeed())
			cleanup(withoutName)

			added := withoutName.DeepCopy()
			added.Spec.DatabaseName = testPGDatabaseName
			Expect(apierrors.IsInvalid(k8sClient.Update(ctx, added))).To(BeTrue())
		})
	})

	Context("Role credentials Secret", func() {
		It("uses the PostgreSQL name as username but keeps the Secret named after the resource", func() {
			cluster := newCluster("names-cluster-" + suffix)
			role := newRole("rs-app-"+suffix, "rs_app")
			role.Spec.ClusterRef.Name = cluster.Name
			Expect(k8sClient.Create(ctx, role)).To(Succeed())
			cleanup(role)

			r := &RoleReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			secretName, err := r.reconcileCredentialsSecret(ctx, role, cluster, nil, "generated", true)
			Expect(err).NotTo(HaveOccurred())
			Expect(secretName).To(Equal(cluster.Name + "-" + role.Name + "-credentials"))

			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: ns}, secret)).To(Succeed())
			Expect(string(secret.Data[SecretKeyUsername])).To(Equal("rs_app"))
			Expect(secret.Labels[LabelAppInstance]).To(Equal(role.Name))
		})
	})

	Context("Database owner resolution", func() {
		r := func() *DatabaseReconciler {
			return &DatabaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		}

		It("returns nil when no owner is set", func() {
			owner, err := r().resolveOwnerRole(ctx, newDatabase("db-"+suffix, "", ""))
			Expect(err).NotTo(HaveOccurred())
			Expect(owner).To(BeNil())
		})

		It("errors when the owner Role does not exist", func() {
			_, err := r().resolveOwnerRole(ctx, newDatabase("db-"+suffix, "", "missing-"+suffix))
			Expect(err).To(MatchError(ContainSubstring("not found")))
		})

		It("errors when the owner Role is not ready, and resolves its PostgreSQL name once ready", func() {
			role := newRole("owner-"+suffix, "rs_owner")
			Expect(k8sClient.Create(ctx, role)).To(Succeed())
			cleanup(role)

			db := newDatabase("db-"+suffix, "", role.Name)
			_, err := r().resolveOwnerRole(ctx, db)
			Expect(err).To(MatchError(ContainSubstring("not ready")))

			role.Status.Ready = true
			Expect(k8sClient.Status().Update(ctx, role)).To(Succeed())

			owner, err := r().resolveOwnerRole(ctx, db)
			Expect(err).NotTo(HaveOccurred())
			Expect(owner.PostgresName()).To(Equal("rs_owner"))
		})

		It("marks the Database not ready with a clear message when the owner is missing", func() {
			cluster := newCluster("owner-cluster-" + suffix)
			cluster.Status.Ready = true
			cluster.Status.SecretName = cluster.Name + "-credentials"
			Expect(k8sClient.Status().Update(ctx, cluster)).To(Succeed())

			// The operator credentials Secret only needs to exist; no
			// connection is attempted before the owner is resolved.
			opSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: cluster.Status.SecretName, Namespace: ns},
				StringData: map[string]string{SecretKeyUsername: DefaultOperatorUsername, SecretKeyPassword: "x"},
			}
			Expect(k8sClient.Create(ctx, opSecret)).To(Succeed())
			cleanup(opSecret)

			db := newDatabase("db-"+suffix, testPGDatabaseName, "missing-"+suffix)
			db.Spec.ClusterRef.Name = cluster.Name
			Expect(k8sClient.Create(ctx, db)).To(Succeed())
			cleanup(db)

			_, err := r().Reconcile(ctx, reconcileRequest(db))
			Expect(err).NotTo(HaveOccurred())

			got := &postgresv1alpha1.Database{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(db), got)).To(Succeed())
			Expect(got.Status.Ready).To(BeFalse())
			Expect(got.Status.DatabaseName).To(BeEmpty())
			Expect(got.Status.Conditions).NotTo(BeEmpty())
			Expect(got.Status.Conditions[0].Message).To(ContainSubstring("owner Role"))
		})

		It("maps an owner Role to the Databases that reference it", func() {
			owned := newDatabase("owned-"+suffix, "", "owner-"+suffix)
			Expect(k8sClient.Create(ctx, owned)).To(Succeed())
			cleanup(owned)
			unrelated := newDatabase("other-"+suffix, "", "someone-else")
			Expect(k8sClient.Create(ctx, unrelated)).To(Succeed())
			cleanup(unrelated)

			reqs := r().databasesForOwnerRole(ctx, newRole("owner-"+suffix, ""))
			Expect(reqs).To(ConsistOf(reconcileRequest(owned)))
		})
	})

	Context("Database credentials Secret", func() {
		r := func() *DatabaseReconciler {
			return &DatabaseReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		}

		It("is skipped when the Database has no owner", func() {
			cluster := newCluster("noowner-cluster-" + suffix)
			db := newDatabase("db-"+suffix, "", "")
			Expect(r().reconcileCredentialsSecret(ctx, db, nil, cluster)).To(Succeed())
		})

		It("is skipped when the owner is a NOLOGIN role", func() {
			cluster := newCluster("nologin-cluster-" + suffix)
			owner := newRole("group-"+suffix, "")
			owner.Spec.Login = new(false)
			db := newDatabase("db-"+suffix, "", owner.Name)
			Expect(r().reconcileCredentialsSecret(ctx, db, owner, cluster)).To(Succeed())
		})

		It("uses the owner's credentials and the effective database name", func() {
			cluster := newCluster("creds-cluster-" + suffix)
			owner := newRole("rs-app-"+suffix, "rs_app")
			Expect(k8sClient.Create(ctx, owner)).To(Succeed())
			cleanup(owner)

			roleSecretName := cluster.Name + "-" + owner.Name + "-credentials"
			roleSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: roleSecretName, Namespace: ns},
				StringData: map[string]string{SecretKeyUsername: "rs_app", SecretKeyPassword: "s3cret"},
			}
			Expect(k8sClient.Create(ctx, roleSecret)).To(Succeed())
			cleanup(roleSecret)

			db := newDatabase("rs-app-db-"+suffix, testPGDatabaseName, owner.Name)
			db.Spec.ClusterRef.Name = cluster.Name
			Expect(k8sClient.Create(ctx, db)).To(Succeed())
			cleanup(db)

			Expect(r().reconcileCredentialsSecret(ctx, db, owner, cluster)).To(Succeed())

			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: db.Name + "-" + owner.Name + "-credentials", Namespace: ns}, secret)).To(Succeed())
			Expect(string(secret.Data[SecretKeyUsername])).To(Equal("rs_app"))
			Expect(string(secret.Data[SecretKeyPassword])).To(Equal("s3cret"))
			Expect(string(secret.Data[SecretKeyDatabase])).To(Equal(testPGDatabaseName))
		})
	})

	Context("Logical backup CronJob", func() {
		It("passes the effective database name via PGDATABASE instead of the script", func() {
			cluster := newCluster("backup-cluster-" + suffix)
			db := newDatabase("rs-app-db-"+suffix, testPGDatabaseName, "")
			db.Spec.ClusterRef.Name = cluster.Name

			backup := &postgresv1alpha1.Backup{
				ObjectMeta: metav1.ObjectMeta{Name: "names-backup-" + suffix, Namespace: ns},
				Spec: postgresv1alpha1.BackupSpec{
					Type: postgresv1alpha1.BackupTypeLogical,
					Destination: postgresv1alpha1.DestinationSpec{
						Type: postgresv1alpha1.DestinationTypeS3,
						S3:   &postgresv1alpha1.S3Destination{Bucket: "b", Region: "us-east-1"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, backup)).To(Succeed())
			cleanup(backup)

			br := &BackupReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			Expect(br.reconcileLogicalCronJob(ctx, backup, cluster, db, cluster.Name+"-credentials",
				"0 2 * * *", postgresv1alpha1.BackupRunTypeData)).To(Succeed())

			cj := &batchv1.CronJob{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: backup.Name + "-data", Namespace: ns}, cj)).To(Succeed())
			dump := cj.Spec.JobTemplate.Spec.Template.Spec.InitContainers[0]
			Expect(dump.Command[2]).To(ContainSubstring(`-d "$PGDATABASE"`))
			Expect(dump.Command[2]).NotTo(ContainSubstring(db.Name))
			Expect(dump.Env).To(ContainElement(corev1.EnvVar{Name: envPGDatabase, Value: testPGDatabaseName}))
		})
	})
})
