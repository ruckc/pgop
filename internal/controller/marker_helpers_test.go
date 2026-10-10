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
	"bytes"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
)

// testSigner signs the markers of the test Cluster "ns/c".
var testSigner = newMarkerSigner(
	&postgresv1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"}},
	bytes.Repeat([]byte{7}, markerKeySize))

// ownerMarker is the test Cluster's valid marker of kind/name.
func ownerMarker(kind, name string) string { return testSigner.marker(kind, name) }
