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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

var _ = Describe("StatefulSet convergence", func() {
	It("does not write an unchanged StatefulSet (no perpetual no-op updates)", func() {
		name := fmt.Sprintf("churn-%d", time.Now().UnixNano())
		c := &postgresv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testReplicationNamespace},
			Spec: postgresv1alpha1.ClusterSpec{Replicas: 2, TLS: &postgresv1alpha1.ClusterTLSSpec{}}}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		updates := 0
		ww, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		cl := interceptor.NewClient(ww, interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*appsv1.StatefulSet); ok {
					updates++
				}
				return cl.Update(ctx, obj, opts...)
			},
		})
		r := &ClusterReconciler{Client: cl, Scheme: k8sClient.Scheme()}
		for range 5 {
			_, _ = r.Reconcile(ctx, reconcileRequest(c))
		}
		Expect(updates).To(BeZero())
	})
})
