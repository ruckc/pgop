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
