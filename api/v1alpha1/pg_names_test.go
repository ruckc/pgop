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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRolePostgresName(t *testing.T) {
	r := &Role{ObjectMeta: metav1.ObjectMeta{Name: "rs-app"}}
	if got := r.PostgresName(); got != "rs-app" {
		t.Errorf("fallback: got %q, want %q", got, "rs-app")
	}
	r.Spec.RoleName = "rs_app"
	if got := r.PostgresName(); got != "rs_app" {
		t.Errorf("override: got %q, want %q", got, "rs_app")
	}
}

func TestDatabasePostgresName(t *testing.T) {
	d := &Database{ObjectMeta: metav1.ObjectMeta{Name: "rs-app-db"}}
	if got := d.PostgresName(); got != "rs-app-db" {
		t.Errorf("fallback: got %q, want %q", got, "rs-app-db")
	}
	d.Spec.DatabaseName = "rs_app_db"
	if got := d.PostgresName(); got != "rs_app_db" {
		t.Errorf("override: got %q, want %q", got, "rs_app_db")
	}
}
