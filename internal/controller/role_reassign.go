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
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// Role deletion with spec.deletion.reassignOwnedTo.
//
// The objects the deleted role owns are handed to the target role in every
// database that accepts connections (REASSIGN OWNED, then DROP OWNED for the
// privileges left over). pgop connects as a superuser, so it re-imposes
// PostgreSQL's rule that the issuer holds the privileges of both roles:
// without it a Role author could reassign SECURITY DEFINER functions to a
// more privileged role.

// checkReassignTarget decides whether the objects of old may be reassigned to
// target. It returns an error with the reason for a refusal.
func checkReassignTarget(old, target string, info postgres.ReassignTargetInfo) error {
	if reason := reservedRoleName(target); reason != "" {
		return fmt.Errorf("cannot reassign owned objects to %q: %s", target, reason)
	}
	if target == old {
		return fmt.Errorf("cannot reassign owned objects of %q to itself", old)
	}
	if !info.Exists {
		return fmt.Errorf("cannot reassign owned objects to %q: the role does not exist", target)
	}
	if info.Super {
		return fmt.Errorf("cannot reassign owned objects to %q: it is a superuser", target)
	}
	if !info.OldHasUsage {
		return fmt.Errorf("cannot reassign owned objects to %q: %q is not a member of it "+
			"(grant the membership first, for example with spec.memberships)", target, old)
	}
	return nil
}

// reassignOwnedObjects hands the objects old owns to target. A non-nil blocked
// error means the reassignment is refused (the drop is blocked with reason
// RoleDropBlocked); skipped lists databases that could not be processed.
func reassignOwnedObjects(ctx context.Context, c client.Client, cluster *postgresv1alpha1.Cluster,
	pg *postgres.Client, old, target string) (skipped []string, blocked error, err error) {
	info, err := pg.RoleReassignTarget(ctx, old, target)
	if err != nil {
		return nil, nil, err
	}
	if err := checkReassignTarget(old, target, info); err != nil {
		return nil, err, nil
	}
	owned, err := pg.DatabasesOwnedBy(ctx, old)
	if err != nil {
		return nil, nil, err
	}
	if len(owned) > 0 {
		return nil, fmt.Errorf("the role owns the database(s) %s, which REASSIGN OWNED would hand to %q; "+
			"change the spec.owner of the Database first", strings.Join(owned, ", "), target), nil
	}
	dbs, err := pg.DatabasesWithOwnedObjects(ctx, old)
	if err != nil {
		return nil, nil, err
	}
	log := logf.FromContext(ctx)
	for _, db := range dbs {
		if !db.AllowConns {
			skipped = append(skipped, db.Name+" (does not allow connections)")
			continue
		}
		if err := reassignOwnedIn(ctx, c, cluster, db.Name, old, target); err != nil {
			log.Error(err, "Failed to reassign owned objects; skipping the database", "database", db.Name)
			skipped = append(skipped, db.Name+" ("+err.Error()+")")
		}
	}
	return skipped, nil, nil
}

func reassignOwnedIn(ctx context.Context, c client.Client, cluster *postgresv1alpha1.Cluster, db, old, target string) error {
	dbClient, err := newOperatorClient(ctx, c, cluster, db)
	if err != nil {
		return err
	}
	defer func() { _ = dbClient.Close() }()
	return dbClient.ReassignOwned(ctx, old, target)
}
