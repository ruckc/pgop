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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RoleMembership declares that this role is a member of another PostgreSQL role.
type RoleMembership struct {
	// role is the PostgreSQL name of the role to be a member of (a raw
	// PostgreSQL role name, not a Role resource name).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Role string `json:"role"`

	// inherit sets the grant's INHERIT option: whether the member automatically
	// uses the privileges of the role. When unset, PostgreSQL's default applies
	// for a new grant (the member's own inherit attribute) and an existing grant
	// is left unchanged. Requires PostgreSQL 16 or later.
	// +optional
	Inherit *bool `json:"inherit,omitempty"`

	// set sets the grant's SET option: whether the member may SET ROLE to the
	// role. When unset, PostgreSQL's default applies for a new grant (true) and
	// an existing grant is left unchanged. Requires PostgreSQL 16 or later.
	// +optional
	Set *bool `json:"set,omitempty"`

	// admin sets the grant's ADMIN option: whether the member may grant
	// membership in the role to others. Defaults to false; a grant that has
	// the ADMIN option while admin is false has it revoked.
	// +optional
	Admin bool `json:"admin,omitempty"`
}

// PasswordRotationSpec configures scheduled rotation of an operator-generated
// role password.
type PasswordRotationSpec struct {
	// every is the rotation interval as a Go duration string (for example
	// "720h" for 30 days). The minimum is 1h. The schedule is measured from
	// status.passwordRotatedAt.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|ms|s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1h')",message="passwordRotation.every must be at least 1h"
	Every metav1.Duration `json:"every"`
}

// RoleSpec defines the desired state of Role
// +kubebuilder:validation:XValidation:rule="!(has(self.passwordSecretRef) && has(self.passwordRotation))",message="passwordRotation cannot be combined with passwordSecretRef; rotate the referenced Secret instead"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.roleName) == has(self.roleName) && (!has(self.roleName) || self.roleName == oldSelf.roleName)",message="roleName is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.memberOf) || !has(self.memberships) || self.memberOf.all(r, !self.memberships.exists(m, m.role == r))",message="a role must not be listed in both memberOf and memberships"
type RoleSpec struct {
	// clusterRef references the PostgreSQL Cluster this role belongs to
	// +kubebuilder:validation:Required
	ClusterRef ClusterReference `json:"clusterRef"`

	// roleName is the name of the role in PostgreSQL. It defaults to
	// metadata.name when unset, and lets the PostgreSQL name use characters
	// (such as underscores) that Kubernetes object names do not allow.
	// It must be a lowercase unquoted identifier, must not start with "pg_",
	// must not be a reserved name (postgres, pgop_operator), and cannot be
	// changed after creation.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('pg_')",message="roleName must not start with 'pg_' (reserved by PostgreSQL)"
	// +kubebuilder:validation:XValidation:rule="!(self in ['postgres', 'pgop_operator'])",message="roleName must not be a reserved role name (postgres, pgop_operator)"
	RoleName string `json:"roleName,omitempty"`

	// login allows the role to log in (connect to the database)
	// +kubebuilder:default=true
	Login *bool `json:"login,omitempty"`

	// superuser grants superuser privileges to the role
	// +optional
	Superuser bool `json:"superuser,omitempty"`

	// createDB allows the role to create new databases
	// +optional
	CreateDB bool `json:"createDB,omitempty"`

	// createRole allows the role to create other roles
	// +optional
	CreateRole bool `json:"createRole,omitempty"`

	// inherit allows the role to inherit privileges from roles it is a member of
	// +kubebuilder:default=true
	Inherit *bool `json:"inherit,omitempty"`

	// replication allows the role to initiate replication connections
	// +optional
	Replication bool `json:"replication,omitempty"`

	// bypassRLS allows the role to bypass row-level security policies
	// +optional
	BypassRLS bool `json:"bypassRLS,omitempty"`

	// connectionLimit sets the maximum number of concurrent connections for this role.
	// -1 means unlimited.
	// +kubebuilder:default=-1
	// +kubebuilder:validation:Minimum=-1
	ConnectionLimit *int32 `json:"connectionLimit,omitempty"`

	// memberOf lists PostgreSQL roles this role should be a member of, granted
	// with PostgreSQL's default options.
	//
	// Deprecated: use memberships instead. memberOf will be removed in a future
	// API version. Each entry is treated as a memberships entry with only role
	// set, and a role must not appear in both fields.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=63
	MemberOf []string `json:"memberOf,omitempty"`

	// memberships lists PostgreSQL roles this role should be a member of, with
	// per-grant options. Memberships that pgop granted and that are later
	// removed from the spec are revoked (see revokeRemovedMemberships);
	// memberships granted outside pgop are never revoked.
	// +optional
	// +listType=map
	// +listMapKey=role
	// +kubebuilder:validation:MaxItems=256
	Memberships []RoleMembership `json:"memberships,omitempty"`

	// revokeRemovedMemberships controls whether memberships that pgop granted
	// (tracked in status.managedMemberships) are revoked once they are removed
	// from memberOf/memberships. Set it to false to keep the legacy grant-only
	// behavior; memberships removed while it is false are no longer tracked and
	// are never revoked later.
	//
	// This is a transitional opt-out: it will be removed together with the
	// deprecated memberOf field, after which removed memberships are always
	// revoked.
	// +kubebuilder:default=true
	// +optional
	RevokeRemovedMemberships *bool `json:"revokeRemovedMemberships,omitempty"`

	// passwordSecretRef references a Secret in the Role's namespace whose key
	// holds the password for this role. When set, the operator uses that value
	// instead of generating one, copies it into the role credentials Secret
	// and follows changes to the referenced Secret. The referenced Secret is
	// never modified or owned by the operator. When the reference is removed,
	// the current password is kept. Mutually exclusive with passwordRotation.
	// +optional
	PasswordSecretRef *SecretKeySelector `json:"passwordSecretRef,omitempty"`

	// passwordRotation enables scheduled rotation of the operator-generated
	// password. A rotation sets a new password in PostgreSQL and then updates
	// the role credentials Secret (and the credentials Secrets of Databases
	// owned by this role). The old password stops working for new connections
	// immediately; existing sessions stay connected. Mutually exclusive with
	// passwordSecretRef.
	// +optional
	PasswordRotation *PasswordRotationSpec `json:"passwordRotation,omitempty"`
}

// RoleStatus defines the observed state of Role.
type RoleStatus struct {
	// ready indicates if the role has been created in the database
	Ready bool `json:"ready,omitempty"`

	// roleName is the effective PostgreSQL role name that was reconciled.
	// +optional
	RoleName string `json:"roleName,omitempty"`

	// secretName is the name of the Secret containing the role's credentials.
	// The secret contains 'username' and 'password' keys.
	SecretName string `json:"secretName,omitempty"`

	// passwordHash is a salted SHA-256 fingerprint of the password last set in
	// PostgreSQL. The operator only sends a new password to PostgreSQL when the
	// desired password's fingerprint differs. It is not the password.
	// +optional
	PasswordHash string `json:"passwordHash,omitempty"`

	// passwordRotatedAt is when the operator last generated the role's
	// password (initially or by rotation). The rotation schedule is measured
	// from it. Unset while the password comes from passwordSecretRef.
	// +optional
	PasswordRotatedAt *metav1.Time `json:"passwordRotatedAt,omitempty"`

	// passwordRotationRequest is the last value of the
	// pgop.ruck.io/rotate-password annotation that the operator acted on.
	// +optional
	PasswordRotationRequest string `json:"passwordRotationRequest,omitempty"`

	// managedMemberships lists the PostgreSQL roles whose membership pgop has
	// granted to this role. Only these are revoked when they are removed from
	// the spec.
	// +optional
	// +listType=set
	ManagedMemberships []string `json:"managedMemberships,omitempty"`

	// conditions represent the current state of the Role resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.clusterRef.name"
// +kubebuilder:printcolumn:name="PGName",type="string",JSONPath=".status.roleName"
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Role is the Schema for the roles API.
// It represents a PostgreSQL role (user) managed by the operator.
type Role struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Role
	// +required
	Spec RoleSpec `json:"spec"`

	// status defines the observed state of Role
	// +optional
	Status RoleStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RoleList contains a list of Role
type RoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Role `json:"items"`
}

// IsLogin reports whether the role may log in (CRD default: true).
func (s *RoleSpec) IsLogin() bool { return s.Login == nil || *s.Login }

// IsInherit reports whether the role inherits privileges (CRD default: true).
func (s *RoleSpec) IsInherit() bool { return s.Inherit == nil || *s.Inherit }

// GetConnectionLimit returns the connection limit (CRD default: -1, unlimited).
func (s *RoleSpec) GetConnectionLimit() int32 {
	if s.ConnectionLimit == nil {
		return -1
	}
	return *s.ConnectionLimit
}

// ShouldRevokeRemovedMemberships reports whether memberships removed from the
// spec are revoked (CRD default: true).
func (s *RoleSpec) ShouldRevokeRemovedMemberships() bool {
	return s.RevokeRemovedMemberships == nil || *s.RevokeRemovedMemberships
}

// DesiredMemberships returns the memberships from memberships followed by the
// deprecated memberOf entries (as memberships with default options). Duplicate
// roles keep their first occurrence.
func (s *RoleSpec) DesiredMemberships() []RoleMembership {
	out := make([]RoleMembership, 0, len(s.Memberships)+len(s.MemberOf))
	seen := make(map[string]bool, cap(out))
	for _, m := range s.Memberships {
		if !seen[m.Role] {
			seen[m.Role] = true
			out = append(out, m)
		}
	}
	for _, r := range s.MemberOf {
		if !seen[r] {
			seen[r] = true
			out = append(out, RoleMembership{Role: r})
		}
	}
	return out
}

// PostgresName returns the role's name in PostgreSQL: spec.roleName when set,
// otherwise metadata.name.
func (r *Role) PostgresName() string {
	if r.Spec.RoleName != "" {
		return r.Spec.RoleName
	}
	return r.Name
}
