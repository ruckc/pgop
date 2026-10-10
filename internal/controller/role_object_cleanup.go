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
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

// Role deletion and object grants.
//
// Privileges on tables, sequences, functions and types and entries in
// pg_default_acl (as grantee, or as the role the default privileges are for)
// block DROP ROLE. While a Role is being deleted, the Databases of its
// Cluster pause the object grants and default privileges that name it, which
// revokes what their ledgers record. The Role controller does not wait for
// that: right before DROP ROLE it revokes, in each such database, exactly
// what the Databases' ledgers (status.managedObjectGrants and
// status.managedDefaultPrivileges) record for the role. Nothing else is
// touched (pgop never runs DROP OWNED): privileges granted outside pgop, and
// objects the role owns, still block the drop, which is reported with
// reason RoleDropBlocked.

// recordedObjectAccess is what one database's ledgers record for a role.
type recordedObjectAccess struct {
	objects  []postgresv1alpha1.ManagedObjectGrant
	defaults []postgresv1alpha1.ManagedDefaultPrivilege
}

// recordedObjectAccessOf returns, per PostgreSQL database of cluster, the
// object grants to role and the default privileges for or to role that the
// Databases of the Cluster record.
func recordedObjectAccessOf(ctx context.Context, c client.Reader, cluster *postgresv1alpha1.Cluster, role string) (
	map[string]*recordedObjectAccess, error) {
	databases := &postgresv1alpha1.DatabaseList{}
	if err := c.List(ctx, databases, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, fmt.Errorf("failed to list Databases: %w", err)
	}
	out := map[string]*recordedObjectAccess{}
	at := func(db string) *recordedObjectAccess {
		if out[db] == nil {
			out[db] = &recordedObjectAccess{}
		}
		return out[db]
	}
	for i := range databases.Items {
		db := &databases.Items[i]
		if db.Spec.ClusterRef.Name != cluster.Name || db.Status.ClusterUID != string(cluster.UID) || db.Status.DatabaseName == "" {
			continue
		}
		for _, g := range db.Status.ManagedObjectGrants {
			if g.Role == role {
				at(db.Status.DatabaseName).objects = append(at(db.Status.DatabaseName).objects, g)
			}
		}
		for _, d := range db.Status.ManagedDefaultPrivileges {
			if d.Role == role || d.ForRole == role {
				at(db.Status.DatabaseName).defaults = append(at(db.Status.DatabaseName).defaults, d)
			}
		}
	}
	return out, nil
}

// objectAccessRevoker is the subset of *postgres.Client used to revoke
// recorded object grants and default privileges.
type objectAccessRevoker interface {
	RevokeOnSchemaObject(ctx context.Context, kind postgres.SchemaObjectKind, schema, identity, grantee string,
		privileges []string, mode postgres.RevokeMode) error
	RevokeDefaultPrivileges(ctx context.Context, t postgres.DefaultPrivilegesTarget, grantee string, privileges []string,
		mode postgres.RevokeMode) error
	RoleOIDs(ctx context.Context, names []string) (map[string]int64, error)
	ListSchemaObjects(ctx context.Context, schema string, kind postgres.SchemaObjectKind,
		sel postgres.SchemaObjectSelector) ([]postgres.SchemaObject, error)
}

var _ objectAccessRevoker = (*postgres.Client)(nil)

// revokeRecordedObjectAccess revokes what rec lists, with CASCADE (the role
// is about to be dropped, so what it passed on goes with it, as with the
// other privileges pgop revokes before DROP ROLE). Default privileges for
// the role are revoked from their grantee; that removes the pg_default_acl
// entry once it is empty. Default privileges to the role are revoked, and so
// are the privileges they gave the role on the objects their forRole created
// in the schema since (what PostgreSQL copied from them into the objects'
// ACLs).
func revokeRecordedObjectAccess(ctx context.Context, pg objectAccessRevoker, role string, rec *recordedObjectAccess) error {
	var errs []error
	for _, g := range rec.objects {
		ki, ok := objectKindOf(g.Kind)
		if !ok {
			continue
		}
		errs = append(errs, revokeRecorded(g.Privileges, g.GrantOptions, func(privs []string, mode postgres.RevokeMode) error {
			return pg.RevokeOnSchemaObject(ctx, ki.pg, g.Schema, g.Object, g.Role, privs, mode)
		}))
	}
	for _, d := range rec.defaults {
		ki, ok := defaultKindOf(d.Kind)
		if !ok {
			continue
		}
		t := postgres.DefaultPrivilegesTarget{ForRole: d.ForRole, Schema: d.Schema, Kind: ki.pg}
		errs = append(errs, revokeRecorded(d.Privileges, d.GrantOptions, func(privs []string, mode postgres.RevokeMode) error {
			return pg.RevokeDefaultPrivileges(ctx, t, d.Role, privs, mode)
		}))
		if d.Role == role {
			errs = append(errs, revokeFromCreatedObjects(ctx, pg, d, ki))
		}
	}
	return errors.Join(errs...)
}

// revokeFromCreatedObjects revokes the privileges the default privileges d
// recorded from d.Role on the objects of the kind d.ForRole owns in d.Schema.
func revokeFromCreatedObjects(ctx context.Context, pg objectAccessRevoker, d postgresv1alpha1.ManagedDefaultPrivilege,
	ki defaultKindInfo) error {
	privs := union(d.Privileges, d.GrantOptions)
	if len(privs) == 0 {
		return nil
	}
	oids, err := pg.RoleOIDs(ctx, []string{d.Role, d.ForRole})
	if err != nil {
		return err
	}
	grantee, ok := oids[d.Role]
	owner, ownerExists := oids[d.ForRole]
	if !ok || !ownerExists {
		return nil
	}
	kinds := []postgres.SchemaObjectKind{ki.object}
	if ki.object == postgres.SchemaFunction {
		kinds = append(kinds, postgres.SchemaProcedure) // ON FUNCTIONS covers procedures
	}
	var errs []error
	for _, kind := range kinds {
		objects, err := pg.ListSchemaObjects(ctx, d.Schema, kind, postgres.SchemaObjectSelector{All: true})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, o := range objects {
			if o.OwnerOID != owner || o.Extension != "" {
				continue
			}
			held, _ := o.Held(grantee)
			if revoke := intersect(privs, held); len(revoke) > 0 {
				errs = append(errs, pg.RevokeOnSchemaObject(ctx, kind, d.Schema, o.Identity, d.Role, revoke,
					postgres.RevokeMode{Cascade: true}))
			}
		}
	}
	return errors.Join(errs...)
}

// revokeRecorded revokes the recorded privileges, and the grant option of
// the recorded grant options whose privilege pgop did not add, with CASCADE.
func revokeRecorded(privileges, grantOptions []string, revoke func([]string, postgres.RevokeMode) error) error {
	var errs []error
	if len(privileges) > 0 {
		errs = append(errs, revoke(privileges, postgres.RevokeMode{Cascade: true}))
	}
	if only := subtract(grantOptions, privileges); len(only) > 0 {
		errs = append(errs, revoke(only, postgres.RevokeMode{GrantOptionOnly: true, Cascade: true}))
	}
	return errors.Join(errs...)
}

// revokeRecordedObjectAccessFor runs revokeRecordedObjectAccess in every
// database whose Database records object grants or default privileges for
// role. It returns the databases where that failed (they are named in the
// RoleDropBlocked condition should DROP ROLE fail).
func revokeRecordedObjectAccessFor(ctx context.Context, c client.Reader, cluster *postgresv1alpha1.Cluster, role string) (
	[]string, error) {
	recorded, err := recordedObjectAccessOf(ctx, c, cluster, role)
	if err != nil {
		return nil, err
	}
	var skipped []string
	for db, rec := range recorded {
		if err := revokeRecordedObjectAccessIn(ctx, c, cluster, db, role, rec); err != nil {
			skipped = append(skipped, db+" ("+err.Error()+")")
		}
	}
	return skipped, nil
}

func revokeRecordedObjectAccessIn(ctx context.Context, c client.Reader, cluster *postgresv1alpha1.Cluster, db, role string,
	rec *recordedObjectAccess) error {
	dbClient, err := newOperatorClient(ctx, c, cluster, db)
	if err != nil {
		return err
	}
	defer func() { _ = dbClient.Close() }()
	return revokeRecordedObjectAccess(ctx, dbClient, role, rec)
}
