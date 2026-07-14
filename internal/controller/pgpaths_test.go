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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

var _ = Describe("parsePostgresMajor", func() {
	DescribeTable("extracts the major version from an image tag",
		func(image string, expected int) {
			Expect(parsePostgresMajor(image)).To(Equal(expected))
		},
		Entry("plain major", "postgres:18", 18),
		Entry("major.minor", "postgres:18.1", 18),
		Entry("major with distro suffix", "postgres:18-bookworm", 18),
		Entry("legacy major", "postgres:17", 17),
		Entry("postgis pg-postgis tag", "postgis/postgis:16-3.4", 16),
		Entry("postgis pg-postgis-distro tag", "postgis/postgis:17-3.5-alpine", 17),
		Entry("registry with port", "myregistry:5000/postgres:18", 18),
		Entry("no tag", "postgres", 0),
		Entry("latest", "postgres:latest", 0),
		Entry("non-numeric tag", "postgres:bookworm", 0),
		Entry("digest pinned, no tag", "postgres@sha256:abc123", 0),
		Entry("tag plus digest", "postgres:18@sha256:abc123", 18),
	)
})

var _ = Describe("layoutForMajor", func() {
	It("uses the data subdir for PG <=17", func() {
		l := layoutForMajor(17)
		Expect(l.MountPath).To(Equal("/var/lib/postgresql/data"))
		Expect(l.PGDATA).To(Equal("/var/lib/postgresql/data"))
	})

	It("uses the parent mount and versioned PGDATA for PG >=18", func() {
		l := layoutForMajor(18)
		Expect(l.MountPath).To(Equal("/var/lib/postgresql"))
		Expect(l.PGDATA).To(Equal("/var/lib/postgresql/18/docker"))
	})

	It("uses the running major in the versioned PGDATA path", func() {
		Expect(layoutForMajor(19).PGDATA).To(Equal("/var/lib/postgresql/19/docker"))
	})
})

var _ = Describe("resolvePostgresLayout", func() {
	It("prefers an explicit spec.postgresMajorVersion over the image tag", func() {
		major := int32(17)
		cluster := &postgresv1alpha1.Cluster{
			Spec: postgresv1alpha1.ClusterSpec{
				Image:                "some-mirror/pg:latest",
				PostgresMajorVersion: &major,
			},
		}
		l, err := resolvePostgresLayout(cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.MountPath).To(Equal("/var/lib/postgresql/data"))
	})

	It("auto-detects from the image tag", func() {
		cluster := &postgresv1alpha1.Cluster{
			Spec: postgresv1alpha1.ClusterSpec{Image: "postgres:18"},
		}
		l, err := resolvePostgresLayout(cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.PGDATA).To(Equal("/var/lib/postgresql/18/docker"))
	})

	It("falls back to the default image when none is set", func() {
		cluster := &postgresv1alpha1.Cluster{}
		l, err := resolvePostgresLayout(cluster)
		Expect(err).NotTo(HaveOccurred())
		Expect(l.MountPath).To(Equal("/var/lib/postgresql"))
	})

	It("fails when the version cannot be determined and no override is set", func() {
		cluster := &postgresv1alpha1.Cluster{
			Spec: postgresv1alpha1.ClusterSpec{Image: "some-mirror/pg:latest"},
		}
		_, err := resolvePostgresLayout(cluster)
		Expect(err).To(HaveOccurred())
	})
})
