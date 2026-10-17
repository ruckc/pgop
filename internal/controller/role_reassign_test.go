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
	"testing"

	"github.com/ruckc/pgop/internal/postgres"
)

func TestCheckReassignTarget(t *testing.T) {
	const target = "owner"
	member := postgres.ReassignTargetInfo{Exists: true, OldHasUsage: true}
	tests := []struct {
		name    string
		old     string
		target  string
		info    postgres.ReassignTargetInfo
		wantErr bool
	}{
		{"member", ogSchema, target, member, false},
		{"not a member", ogSchema, target, postgres.ReassignTargetInfo{Exists: true}, true},
		{"superuser", ogSchema, target, postgres.ReassignTargetInfo{Exists: true, Super: true, OldHasUsage: true}, true},
		{"missing", ogSchema, target, postgres.ReassignTargetInfo{}, true},
		{"self", ogSchema, ogSchema, member, true},
		{defaultDatabaseName, ogSchema, defaultDatabaseName, member, true},
		{"operator role", ogSchema, DefaultOperatorUsername, member, true},
		{"predefined", ogSchema, reservedPgPrefixed, member, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkReassignTarget(tt.old, tt.target, tt.info)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
