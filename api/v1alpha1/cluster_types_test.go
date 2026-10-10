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
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// TestReservedParametersMatchCRD checks that the CEL rule rejecting reserved
// spec.parameters names lists exactly ReservedParameters, so the two cannot
// drift apart when a reservation is added or relaxed.
func TestReservedParametersMatchCRD(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/bases/pgop.ruck.io_clusters.yaml")
	if err != nil {
		t.Fatal(err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(raw, crd); err != nil {
		t.Fatal(err)
	}
	params := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["parameters"]
	var rule string
	for _, v := range params.XValidations {
		if strings.Contains(v.Rule, "lowerAscii") {
			rule = v.Rule
		}
	}
	if rule == "" {
		t.Fatal("no reserved-parameter rule on spec.parameters")
	}
	matches := regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(rule, -1)
	inRule := make([]string, 0, len(matches))
	for _, m := range matches {
		inRule = append(inRule, m[1])
	}
	want := slices.Sorted(slices.Values(ReservedParameters))
	slices.Sort(inRule)
	if !slices.Equal(inRule, want) {
		t.Errorf("CEL rule lists %v, ReservedParameters is %v", inRule, want)
	}
	for _, name := range ReservedParameters {
		if name != strings.ToLower(name) {
			t.Errorf("ReservedParameters must be lower case: %q", name)
		}
	}
}

// TestGrantablePredefinedRolesMatchCRD checks that the enum on
// spec.rolePolicy.allowedPredefinedRoles lists exactly
// GrantablePredefinedRoles, and that none of them is always forbidden.
func TestGrantablePredefinedRolesMatchCRD(t *testing.T) {
	raw, err := os.ReadFile("../../config/crd/bases/pgop.ruck.io_clusters.yaml")
	if err != nil {
		t.Fatal(err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(raw, crd); err != nil {
		t.Fatal(err)
	}
	field := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["rolePolicy"].Properties["allowedPredefinedRoles"]
	if field.Items == nil || field.Items.Schema == nil {
		t.Fatal("allowedPredefinedRoles has no item schema")
	}
	inEnum := make([]string, 0, len(field.Items.Schema.Enum))
	for _, v := range field.Items.Schema.Enum {
		inEnum = append(inEnum, strings.Trim(string(v.Raw), `"`))
	}
	slices.Sort(inEnum)
	want := slices.Sorted(slices.Values(GrantablePredefinedRoles))
	if !slices.Equal(inEnum, want) {
		t.Errorf("CRD enum lists %v, GrantablePredefinedRoles is %v", inEnum, want)
	}
	for _, name := range ForbiddenPredefinedRoles {
		if slices.Contains(GrantablePredefinedRoles, name) {
			t.Errorf("%s is both grantable and forbidden", name)
		}
	}
}

func TestRolePolicyAllows(t *testing.T) {
	var none *RolePolicySpec
	if none.AllowsAttribute(RoleAttributeCreateRole) || none.AllowsPredefinedRole("pg_monitor") || none.AllowsExtension("x") {
		t.Error("a nil policy must allow nothing")
	}
	p := &RolePolicySpec{
		AllowedAttributes:      []RoleAttribute{RoleAttributeBypassRLS},
		AllowedPredefinedRoles: []string{"pg_monitor", "pg_execute_server_program"},
		AllowedExtensions:      []string{"file_fdw"},
	}
	if !p.AllowsAttribute(RoleAttributeBypassRLS) || p.AllowsAttribute(RoleAttributeReplication) {
		t.Error("allowedAttributes not honored")
	}
	if !p.AllowsPredefinedRole("pg_monitor") || p.AllowsPredefinedRole("pg_execute_server_program") {
		t.Error("allowedPredefinedRoles must only allow grantable roles")
	}
	if !p.AllowsExtension("file_fdw") || p.AllowsExtension("dblink") {
		t.Error("allowedExtensions not honored")
	}
}
