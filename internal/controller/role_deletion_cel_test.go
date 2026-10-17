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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

const reservedPgPrefixed = "pg_x"

// CRD validation of Role spec.deletion.reassignOwnedTo.
var _ = Describe("Role deletion CRD validation", func() {
	const ns = "default"

	var (
		ctx    context.Context
		suffix string
	)

	BeforeEach(func() {
		ctx = context.Background()
		suffix = fmt.Sprintf("%d", time.Now().UnixNano())
	})

	create := func(spec postgresv1alpha1.RoleSpec) error {
		spec.ClusterRef = postgresv1alpha1.ClusterReference{Name: nonexistentCluster}
		obj := &postgresv1alpha1.Role{ObjectMeta: metav1.ObjectMeta{Name: "del-" + suffix, Namespace: ns}, Spec: spec}
		err := k8sClient.Create(ctx, obj)
		if err == nil {
			DeferCleanup(func() {
				obj.SetFinalizers(nil)
				_ = client.IgnoreNotFound(k8sClient.Update(ctx, obj))
				_ = k8sClient.Delete(ctx, obj)
			})
		}
		return err
	}
	deletion := func(target string) postgresv1alpha1.RoleSpec {
		return postgresv1alpha1.RoleSpec{Deletion: &postgresv1alpha1.RoleDeletionSpec{ReassignOwnedTo: target}}
	}

	for _, target := range []string{defaultDatabaseName, "pgop_x", reservedPgPrefixed, strings.Repeat("a", 64)} {
		It(fmt.Sprintf("rejects reassignOwnedTo %q", target), func() {
			Expect(apierrors.IsInvalid(create(deletion(target)))).To(BeTrue())
		})
	}

	It("rejects the role itself as the target", func() {
		spec := deletion("app_x")
		spec.RoleName = "app_x"
		Expect(apierrors.IsInvalid(create(spec))).To(BeTrue())
	})

	It("accepts a valid target", func() {
		Expect(create(deletion("app_owner"))).To(Succeed())
	})

	It("accepts an empty deletion", func() {
		Expect(create(postgresv1alpha1.RoleSpec{Deletion: &postgresv1alpha1.RoleDeletionSpec{}})).To(Succeed())
	})
})
