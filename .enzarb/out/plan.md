# Plan: finish #24 — `Role.spec.deletion.reassignOwnedTo` and docs cleanup

## Summary
Most of #24 is already on `main`: database grants and settings, `Role.parameterGrants`, role policy, role settings, the grant engine, extension grants, object grants and default privileges. `git grep -i reassign` finds nothing in `api/` or `internal/`, so the only unimplemented item from the agreed plan is PR6: an opt-in `Role.spec.deletion.reassignOwnedTo`. I will add it, then reword the docs that still say "see #24". This is additive. When `deletion` is unset, behaviour does not change.

## Approach

**API** (`api/v1alpha1/role_types.go`):
```go
type RoleDeletionSpec struct {
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=63
    // +kubebuilder:validation:XValidation:rule="self != 'postgres' && !self.startsWith('pgop_')", message=...
    ReassignOwnedTo string `json:"reassignOwnedTo,omitempty"`
}
// RoleSpec: Deletion *RoleDeletionSpec `json:"deletion,omitempty"`
```
- The name is the raw PostgreSQL role name, the same convention as `memberships[].role`.
- I will also add `!self.startsWith('pg_')`. PostgreSQL rejects `pg_*` targets anyway, so this only fails earlier.
- The CRD forbids a role from naming itself as the target. I will do that with a CEL check against `roleName` / `metadata.name` where expressible, and re-check in Go.

**Deletion flow.** In `RoleReconciler.dropPostgresRole` (`role_controller.go`), after the existing ownership and marker checks and the ledger-based revokes, and before `DropRole`:
1. If `spec.deletion.reassignOwnedTo` is empty, skip this step. The existing path is unchanged.
2. **Guards.** Do all of these before any SQL that changes data:
   - Refuse a reserved target (`reservedRoleName`: `postgres`, `pgop_*`, `pg_*`) and a target equal to the old role.
   - Refuse a target that does not exist, or has `rolsuper`. This is a new `Client.RoleReassignTarget` query on `pg_roles`.
   - Require `pg_has_role(old, target, 'USAGE')`. pgop connects as superuser, so this re-imposes PostgreSQL's rule that the issuer must hold privileges of both roles. Without it, a Role writer could escalate by reassigning `SECURITY DEFINER` functions to a more privileged role.
   - On refusal, set `Available=False` with reason `RoleDropBlocked` and an explanatory message, and requeue after 30s (the existing blocked-drop pattern). Nothing is reassigned, and the finalizer stays.
3. **Find databases.** Add `Client.DatabasesWithOwnedObjects(role)`. It reads `pg_shdepend` with `deptype = 'o'` and `refobjid = old`, joined to `pg_database` for `datallowconn`. It returns the same `DatabaseRef` type as `DatabasesWithObjectPrivileges`.
4. **Reassign in each database.** For each connectable database, open `newOperatorClient(..., db)` and run `Client.ReassignOwned(ctx, old, target)`. This issues `REASSIGN OWNED BY <old> TO <target>`, then `DROP OWNED BY <old>`, with identifiers going through `quoteIdent`.
   - Do both in one transaction per database. The DROP is not reached if the REASSIGN fails.
   - The order is fixed: `DROP OWNED` runs only after the reassignment, so it removes just the leftover ACL entries and default privileges.
   - Databases that do not allow connections, or cannot be reached, are skipped and listed in the `RoleDropBlocked` message, as `skipped` does today. They never wedge the finalizer.
5. **Shared objects.** `REASSIGN OWNED` also moves ownership of databases and tablespaces owned by the role. A pgop `Database` owner would be silently changed. So if the role owns a database (`pg_shdepend`, `dbid = 0`, `classid = pg_database`), refuse with `RoleDropBlocked`, naming the database. Tell the user to change `Database.spec.owner` first. Revisit this if the maintainer prefers otherwise (see questions).
6. The existing `DropRole` and `DependentObjectsError` handling then runs unchanged. Anything the reassign could not handle is reported there.

**Alternatives rejected**
- Running `DROP OWNED` alone. It would delete user data.
- Defaulting the target (for example to the Database owner). A silent ownership transfer is the wrong default.
- Reassigning on `Database.spec.schemas[].owner` change. That was explicitly excluded in the plan. I will just document it.
- A new durable ledger. The Role needs no new status fields, because the work is idempotent: `REASSIGN` and `DROP OWNED` are safe to retry.

## Changes
- `api/v1alpha1/role_types.go`: add `RoleDeletionSpec` and `RoleSpec.Deletion`, with CEL. Then run `make manifests generate`. This regenerates `config/crd/bases/*`, the deepcopy file and `config/rbac/role.yaml`, which I will not edit by hand. I will also check whether the Helm chart vendors the CRDs separately.
- `internal/postgres/client.go` (or a new `internal/postgres/role_reassign.go`, matching `role_cleanup.go`): `RoleReassignTarget`, `DatabasesWithOwnedObjects`, `DatabasesWithOwnedShared`, `ReassignOwned`, and a pure `buildReassignOwnedQueries` builder.
- `internal/controller/role_reassign.go` (new): the guard decision as a pure function, `checkReassignTarget(old, target string, info ...) error`, and the per-database loop. This follows the shape of `role_object_cleanup.go`.
- `internal/controller/role_controller.go`: call the reassign step from `dropPostgresRole`, and add its `skipped` entries to the blocked message.
- `internal/controller/constants.go`: reuse `ReasonRoleDropBlocked`. No new reason.
- Docs:
  - `docs/user-guide/roles.md`: add a "Deleting a Role that owns objects" section, and replace the manual `REASSIGN OWNED` advice near line 555 with the new field. Document the membership requirement, the refusals, the shared-object limitation, and that `DROP OWNED` follows the reassignment.
  - `docs/reference/api.md`: document the field.
  - `docs/user-guide/databases.md` (about line 281): replace "There is no arbitrary SQL (`initSQL`): pgop declares grants instead (issue #24)" with a statement of what is now declarative. State that `LEAKPROOF`, event triggers, and superuser-owned C or `SECURITY DEFINER` functions remain bootstrap tasks.
  - `config/samples/postgres_v1alpha1_role.yaml`: add an example (commented if needed so the sample stays valid).

## Tests
- **Unit, `internal/postgres`**: query builders (quoting of hostile names, ordering of REASSIGN before DROP).
- **Unit, `internal/controller`**: a table test for `checkReassignTarget`:
  - the old role is a member of the target → allowed;
  - not a member → refused;
  - the target is a superuser or reserved → refused;
  - the target is missing → refused;
  - the target equals the old role → refused.
  
  Add fake-client tests for the `RoleDropBlocked` condition and for skipping a database that does not allow connections.
- **envtest CEL**: `reassignOwnedTo` values `postgres`, `pgop_x`, `pg_x` and a name over 63 characters are rejected. A valid name is accepted, and an empty `deletion` is accepted.
- **e2e** (`test/e2e/`, alongside `role_policy_test.go` or `grants_test.go`):
  - A role that is a member of the target owns a table. Deleting it with `reassignOwnedTo` succeeds, and the table survives under the new owner.
  - Deleting a role that is not a member of the target is blocked with `RoleDropBlocked`.
- **Checks**: `make manifests generate`, `make lint`, `make test`, and `make test-e2e` where a Kind cluster is available. I have not run any of these yet.

## Risks and open questions
- **Escalation through `SECURITY DEFINER`.** The membership check and the reserved/superuser refusal are the safeguards. They must run in Go on the server's current state, not only in CEL.
- **Shared objects.** Decision for the maintainer: refuse when the role owns a database (planned), or allow `REASSIGN OWNED` to move it?
- **Partial progress.** Per-database transactions make retries safe, but the databases are not all-or-nothing. A retry continues where it stopped.
- **Unreachable databases.** They are skipped with the reason in the condition. `DROP ROLE` then fails and the blocked-drop retry applies, as today.
- **Cluster-level policy.** I assume no `Cluster.rolePolicy` switch is needed for this, because the membership check already bounds the field. Say if you want one.
- I have only read the code. I have not run any of the checks above.

## Out of scope
- Event-trigger bindings (PR7). The maintainer has not decided whether to build them, so they would be a separate issue.
- `LEAKPROOF`, `initSQL`, column-level privileges, a `rawGrants` field, and a `Grant` CR.
- Any new durable grant ledger.
- Reassigning when a schema owner changes.

Reply with feedback to revise this plan, or comment /approve to start the implementation.
