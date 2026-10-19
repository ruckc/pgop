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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

var _ = Describe("Cluster volume-permissions init container", func() {
	const ClusterNamespace = "default"
	ctx := context.Background()

	reconcileCluster := func(image string) (*ClusterReconciler, types.NamespacedName, *postgresv1alpha1.Cluster) {
		name := fmt.Sprintf("test-volperm-%d", time.Now().UnixNano())
		cluster := &postgresv1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ClusterNamespace},
			Spec:       postgresv1alpha1.ClusterSpec{Image: image},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() {
			cluster.Finalizers = nil
			_ = k8sClient.Update(ctx, cluster)
			_ = k8sClient.Delete(ctx, cluster)
		})
		r := &ClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		nn := types.NamespacedName{Name: name, Namespace: ClusterNamespace}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		return r, nn, cluster
	}

	for _, image := range []string{"postgres:16", "postgres:17"} {
		It("adds a minimal-capability root init container for "+image, func() {
			_, nn, _ := reconcileCluster(image)
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, nn, sts)).To(Succeed())
			spec := sts.Spec.Template.Spec

			Expect(spec.InitContainers).To(HaveLen(1))
			ic := spec.InitContainers[0]
			Expect(ic.Name).To(Equal("volume-permissions"))
			Expect(*ic.SecurityContext.RunAsUser).To(Equal(int64(0)))
			Expect(*ic.SecurityContext.RunAsNonRoot).To(BeFalse())
			Expect(*ic.SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
			Expect(ic.SecurityContext.Capabilities.Drop).To(ConsistOf(corev1.Capability("ALL")))
			Expect(ic.SecurityContext.Capabilities.Add).To(ConsistOf(
				corev1.Capability("CHOWN"), corev1.Capability("FOWNER"), corev1.Capability("DAC_OVERRIDE")))
			Expect(ic.VolumeMounts).To(HaveLen(1))

			pg := spec.Containers[0]
			Expect(*pg.SecurityContext.RunAsNonRoot).To(BeTrue())
			Expect(*pg.SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
			Expect(pg.SecurityContext.Capabilities.Drop).To(ConsistOf(corev1.Capability("ALL")))
			Expect(pg.SecurityContext.Capabilities.Add).To(BeEmpty())
			Expect(spec.SecurityContext.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
			Expect(*spec.SecurityContext.FSGroup).To(Equal(int64(999)))
		})
	}

	It("does not add the init container for PG18", func() {
		_, nn, _ := reconcileCluster("postgres:18")
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, nn, sts)).To(Succeed())
		Expect(sts.Spec.Template.Spec.InitContainers).To(BeEmpty())
	})

	It("backfills the init container on an existing StatefulSet and then is a no-op", func() {
		r, nn, _ := reconcileCluster("postgres:16")
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, nn, sts)).To(Succeed())
		sts.Spec.Template.Spec.InitContainers = nil
		Expect(k8sClient.Update(ctx, sts)).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, nn, sts)).To(Succeed())
		Expect(sts.Spec.Template.Spec.InitContainers).To(HaveLen(1))
		Expect(sts.Spec.Template.Spec.InitContainers[0].Name).To(Equal("volume-permissions"))

		gen := sts.Generation
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, nn, sts)).To(Succeed())
		Expect(sts.Generation).To(Equal(gen))
	})

	It("reports the volume fix only for layouts mounted at PGDATA", func() {
		Expect(layoutForMajor(16).NeedsVolumeFix()).To(BeTrue())
		Expect(layoutForMajor(17).NeedsVolumeFix()).To(BeTrue())
		Expect(layoutForMajor(18).NeedsVolumeFix()).To(BeFalse())
	})
})
