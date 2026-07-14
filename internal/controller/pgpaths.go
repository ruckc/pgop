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
	"fmt"
	"strconv"
	"strings"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

// postgresLayout describes where the operator mounts the data PVC and where the
// official postgres container image actually stores its data directory (PGDATA),
// for a given PostgreSQL major version.
//
// The official image changed its convention in PostgreSQL 18:
//
//	PG <= 17: VOLUME and PGDATA are both /var/lib/postgresql/data
//	PG >= 18: VOLUME is /var/lib/postgresql, PGDATA is /var/lib/postgresql/<major>/docker
//
// The 18+ layout mounts the parent directory so that multiple major versions'
// data directories (e.g. .../17/docker and .../18/docker) share one volume,
// enabling `pg_upgrade --link`. See
// https://github.com/docker-library/docs/blob/master/postgres/README.md
type postgresLayout struct {
	// MountPath is the path at which the data PVC is mounted in the container.
	MountPath string
	// PGDATA is the actual PostgreSQL data directory, set explicitly via the
	// PGDATA env var so behavior does not depend on the image's own default.
	PGDATA string
}

// layoutForMajor returns the mount path and PGDATA for the given PostgreSQL
// major version, following the official image conventions described above.
func layoutForMajor(major int) postgresLayout {
	if major >= 18 {
		return postgresLayout{
			MountPath: "/var/lib/postgresql",
			PGDATA:    fmt.Sprintf("/var/lib/postgresql/%d/docker", major),
		}
	}
	return postgresLayout{
		MountPath: "/var/lib/postgresql/data",
		PGDATA:    "/var/lib/postgresql/data",
	}
}

// parsePostgresMajor extracts the leading PostgreSQL major version from a
// container image reference's tag. It handles the common tag shapes:
//
//	postgres:18, postgres:18.1, postgres:18-bookworm    -> 18
//	postgis/postgis:16-3.4, postgis/postgis:17-3.5-alpine -> 16, 17
//
// It returns 0 when the version cannot be determined (no tag, a non-numeric tag
// such as "latest", or a digest-pinned reference).
func parsePostgresMajor(image string) int {
	// Strip a digest (e.g. postgres:18@sha256:...) — the tag, if any, precedes it.
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}

	// The tag follows the last ':' that comes after the last '/'. A ':' before
	// the last '/' is a registry port (e.g. myregistry:5000/postgres), not a tag.
	colon := strings.LastIndex(image, ":")
	if colon < 0 || colon < strings.LastIndex(image, "/") {
		return 0
	}
	tag := image[colon+1:]

	// Take the leading run of digits (e.g. "18" from "18-bookworm" or "18.1").
	end := 0
	for end < len(tag) && tag[end] >= '0' && tag[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	major, err := strconv.Atoi(tag[:end])
	if err != nil {
		return 0
	}
	return major
}

// resolvePostgresLayout determines the data-directory layout for a Cluster.
//
// An explicit spec.postgresMajorVersion always wins; otherwise the major version
// is parsed from the (defaulted) image tag. It returns an error when the version
// cannot be determined, so the reconcile fails with a clear condition rather than
// guessing a layout and risking data loss.
func resolvePostgresLayout(cluster *postgresv1alpha1.Cluster) (postgresLayout, error) {
	if cluster.Spec.PostgresMajorVersion != nil {
		return layoutForMajor(int(*cluster.Spec.PostgresMajorVersion)), nil
	}

	image := cluster.Spec.Image
	if image == "" {
		image = DefaultPostgresImage
	}
	major := parsePostgresMajor(image)
	if major == 0 {
		return postgresLayout{}, fmt.Errorf(
			"cannot determine PostgreSQL major version from image %q; set spec.postgresMajorVersion explicitly",
			image)
	}
	return layoutForMajor(major), nil
}
