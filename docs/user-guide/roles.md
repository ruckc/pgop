# Roles

A Role resource represents a PostgreSQL role (user) within a cluster.

## Overview

The Role controller:

1. Connects to the referenced PostgreSQL cluster
2. Creates or updates the role with specified permissions
3. Auto-generates a password and creates a credentials Secret
4. Manages role memberships (GRANT, option changes, and REVOKE of memberships it granted)
5. Cleans up the role on deletion

## Example

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: app-user
  namespace: default
spec:
  clusterRef:
    name: my-cluster
  login: true
  createDB: false
  connectionLimit: 100
  memberships:
    - role: app_read_role
```

## Spec Reference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `clusterRef.name` | string | **required** | Name of the Cluster resource (same namespace) |
| `roleName` | string | `metadata.name` | Role name in PostgreSQL (see [PostgreSQL Role Name](#postgresql-role-name)) |
| `login` | bool | `false` | Can role log in? |
| `superuser` | bool | `false` | Grant superuser privileges |
| `createDB` | bool | `false` | Can role create databases? |
| `createRole` | bool | `false` | Can role create other roles? |
| `inherit` | bool | `true` | Inherit privileges from member roles |
| `replication` | bool | `false` | Can role initiate replication? |
| `bypassRLS` | bool | `false` | Bypass row-level security? |
| `connectionLimit` | int | `-1` | Max concurrent connections (-1 = unlimited) |
| `memberships` | []RoleMembership | - | Roles this role is a member of, with grant options (see [Memberships](#memberships)) |
| `memberOf` | []string | - | **Deprecated**, use `memberships`. Roles this role is a member of |
| `revokeRemovedMemberships` | bool | `true` | Revoke pgop-granted memberships removed from the spec (transitional opt-out) |
| `passwordSecretRef` | SecretKeySelector | - | Take the password from a key of a Secret you manage (see [Password Source](#password-source)) |
| `passwordRotation.every` | duration | - | Rotate the generated password on this interval, e.g. `720h` (minimum `1h`; see [Password Rotation](#password-rotation)) |

## Status

| Field | Description |
|-------|-------------|
| `ready` | Whether the role exists in PostgreSQL |
| `roleName` | The effective PostgreSQL role name that was reconciled |
| `secretName` | Name of the auto-generated credentials secret (`<cluster>-<role>-credentials`) |
| `managedMemberships` | PostgreSQL roles whose membership pgop granted to this role |
| `passwordHash` | Salted SHA-256 fingerprint of the password last set in PostgreSQL (not the password) |
| `passwordRotatedAt` | When the operator last generated the password; the rotation schedule counts from it |
| `passwordRotationRequest` | Last `pgop.ruck.io/rotate-password` annotation value acted on |
| `conditions` | Detailed status conditions |

## Credentials Secret

The operator creates a Secret named **`<cluster-name>-<role-name>-credentials`**
(e.g. `my-cluster-app-user-credentials`). The name is deterministic — it is
derived from `clusterRef.name` and the Role's own name — so you can reference it
from other manifests (including a Helm chart at template time) **before**
`status.secretName` is populated.

```yaml
data:
  username: app-user                          # = the PostgreSQL role name
  password: <generated, or copied from passwordSecretRef>
  host: my-cluster.default.svc.cluster.local
  port: "5432"
  sslmode: disable                            # verify-full once Cluster TLS is active
  uri: postgresql://app-user:<password>@my-cluster.default.svc.cluster.local:5432/postgres?sslmode=disable
  ca.crt: <PEM>                               # only while Cluster TLS is active
```

!!! note
    This Secret does **not** contain a `database` key, and its `uri` points at
    the `postgres` maintenance database. Apps should usually use the
    [Database](databases.md) per-database Secret, whose `database` and `uri`
    name the application database.

The connection keys (`host`, `port`, `sslmode`, `uri`, `ca.crt`) follow the
Cluster: they are updated when its port or [TLS](clusters.md#tls) state
changes. The `password` (and the `uri` built from it) changes only when the
[password source](#password-source) changes it: a new value in the
`passwordSecretRef` Secret, or a [rotation](#password-rotation).
Database credentials Secrets of Databases owned by the Role follow it.

## PostgreSQL Role Name

By default the PostgreSQL role is named after the Role resource
(`metadata.name`). Kubernetes names cannot contain underscores, so set
`spec.roleName` when the PostgreSQL name should differ:

```yaml
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: rs-app            # Kubernetes name (Secret names, labels, references)
spec:
  clusterRef:
    name: my-cluster
  roleName: rs_app        # PostgreSQL role name
```

- `roleName` must match `^[a-z_][a-z0-9_]*$` (lowercase, so it never needs
  quoting), be at most 63 characters, must not start with `pg_` (reserved by
  PostgreSQL), and must not be `postgres` or `pgop_operator`.
- It is **immutable**: it cannot be added, changed or removed after the Role is
  created. To rename, create a new Role.
- Kubernetes-side names still use `metadata.name`: the credentials Secret is
  `<cluster>-rs-app-credentials`, and a Database refers to this Role with
  `owner: rs-app`. The Secret's `username` key holds `rs_app`.
- `memberships[].role` and `memberOf` entries are raw PostgreSQL role names (for example `rs_app`), not
  Role resource names.

!!! warning
    Two Role resources on the same cluster that resolve to the same PostgreSQL
    name are not detected yet; deleting either one drops the shared role.

## Memberships

`spec.memberships` makes this role a member of other PostgreSQL roles
(`GRANT <role> TO <this role>`), with per-grant options:

```yaml
spec:
  clusterRef:
    name: my-cluster
  memberships:
    - role: app_read_role          # plain membership, PostgreSQL defaults
    - role: app_owner
      inherit: false               # must SET ROLE app_owner to use its privileges
      set: true
    - role: app_admins
      admin: true                  # may grant app_admins to others
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `role` | string | **required** | PostgreSQL name of the role to be a member of |
| `inherit` | bool | PostgreSQL default | `INHERIT` grant option: use the role's privileges automatically |
| `set` | bool | PostgreSQL default | `SET` grant option: allow `SET ROLE` to the role |
| `admin` | bool | `false` | `ADMIN` grant option: allow granting the role to others |

- `inherit` and `set` require **PostgreSQL 16 or later**. On an older server,
  a Role that sets them fails with a clear `ReconcileError` condition; plain
  and `admin` memberships still work.
- When `inherit` or `set` is unset, a new grant gets PostgreSQL's default
  (`inherit` follows this role's `inherit` attribute, `set` is true) and the
  option of an existing grant is left as it is.
- Changing an option updates the existing grant. Setting `admin` back to
  `false` (or omitting it) runs `REVOKE ADMIN OPTION FOR`.
- Each role may appear only once in `memberships`.

### Revoking removed memberships

pgop records the memberships it manages in `status.managedMemberships`.
When an entry is removed from `memberships` (or `memberOf`), pgop revokes that
membership. Memberships granted outside pgop (for example by a DBA with
`GRANT`) are never revoked, because they were never in
`status.managedMemberships`.

Things to know:

- **Upgrading:** existing Roles have no `status.managedMemberships` yet. The
  first reconcile after upgrading records the current spec without revoking
  anything, so a membership removed from the spec *before* the upgrade stays
  in place. Revoke it manually if needed.
- **Status loss:** if the status is lost (the Role is deleted and recreated,
  or restored from a backup without status), memberships removed in the
  meantime are not revoked.
- Listing a role that a DBA granted manually adopts it: it becomes managed and
  is revoked when later removed from the spec.

To keep the previous grant-only behavior, set `revokeRemovedMemberships:
false`. Memberships removed while it is `false` are dropped from
`status.managedMemberships` without being revoked, and are not revoked later
if the setting is turned back on.

!!! warning
    `revokeRemovedMemberships` is a transitional opt-out. It will be removed
    together with the deprecated `memberOf` field, after which removed
    memberships are always revoked.

### Deprecated `memberOf`

`spec.memberOf` is deprecated in favor of `spec.memberships` and will be
removed in a future API version. Each `memberOf` entry behaves like a
`memberships` entry with only `role` set. A role must not be listed in both
fields; the API server rejects such a Role. To migrate, move each entry:

```yaml
# before
memberOf:
  - app_read_role
# after
memberships:
  - role: app_read_role
```

Moving an entry from `memberOf` to `memberships` in a single update does not
revoke and re-grant it.

## Role Types

### Application User

```yaml
spec:
  clusterRef:
    name: my-cluster
  login: true
  connectionLimit: 50
```

### Read-Only Role

```yaml
spec:
  clusterRef:
    name: my-cluster
  login: false  # Group role, not a login
  inherit: true
```

### Admin Role

```yaml
spec:
  clusterRef:
    name: my-cluster
  login: true
  createDB: true
  createRole: true
```

## Password Source

The credentials Secret above is always the place apps read the password from,
whichever source it comes from:

1. **`passwordSecretRef` set**: the value of `data[key]` of the named Secret in
   the Role's namespace. The operator watches that Secret: a change is applied
   to PostgreSQL and copied into the credentials Secret (and the Database
   credentials Secrets). The operator never modifies or owns the referenced
   Secret. If the Secret or key is missing (or empty), the Role reports
   `Available=False` with reason `PasswordSecretNotFound` and the operator does
   **not** fall back to a generated password.
2. **Otherwise**: the operator generates a random password when the Role is
   first created and keeps it, unless [rotation](#password-rotation) replaces
   it.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: app-user-password
stringData:
  password: s3cr3t-from-vault
---
apiVersion: pgop.ruck.io/v1alpha1
kind: Role
metadata:
  name: app-user
spec:
  clusterRef:
    name: my-cluster
  passwordSecretRef:
    name: app-user-password
    key: password
```

When `passwordSecretRef` is removed from a Role, the current password is kept
(it stays in the credentials Secret); the operator does not generate a new one
until a rotation is due or requested.

The operator sends `ALTER ROLE ... PASSWORD` to PostgreSQL only when the
password changed (it compares a salted fingerprint stored in
`status.passwordHash`), so the password does not show up in server logs on
every reconcile with `log_statement=ddl`. The operator never logs passwords.

!!! note
    Editing `password` in the operator-managed credentials Secret directly sets
    that password in PostgreSQL on the next reconcile, unless
    `passwordSecretRef` is set, in which case the referenced value wins.

## Password Rotation

`spec.passwordRotation` rotates the operator-generated password on a schedule:

```yaml
spec:
  clusterRef:
    name: my-cluster
  passwordRotation:
    every: 720h   # 30 days; Go duration, minimum 1h
```

When `status.passwordRotatedAt + every` has passed, the operator generates a
new password, sets it in PostgreSQL, then writes it to the credentials Secret
and the Database credentials Secrets, records `status.passwordRotatedAt` and
emits a `PasswordRotated` Event. If the Secret write fails, the next reconcile
rotates again, so apps are never handed a password PostgreSQL does not know.
For Roles created before rotation was enabled, the schedule starts when the
operator first sees `passwordRotation`.

`passwordRotation` and `passwordSecretRef` are mutually exclusive (the API
rejects a Role with both). With `passwordSecretRef`, rotate the referenced
Secret yourself (e.g. with your secret manager); the operator follows it.

### Rotating on demand

Set the `pgop.ruck.io/rotate-password` annotation to a new value (any string,
for example a timestamp or an incident ID) to rotate immediately, with or
without `passwordRotation`:

```bash
kubectl annotate role.pgop.ruck.io app-user --overwrite \
  pgop.ruck.io/rotate-password="$(date +%s)"
```

The operator acts on each value once and records it in
`status.passwordRotationRequest`. On a Role with `passwordSecretRef` the
annotation does not generate a password; it sets the referenced password in
PostgreSQL again (useful after a physical restore brought back an older
password).

### What a rotation means for apps

PostgreSQL has a single password per role, so there is no overlap period:

- **New connections** must use the new password as soon as it is set.
- **Existing sessions** stay connected; PostgreSQL only checks the password
  when a connection is opened.
- Apps must re-read the Secret before reconnecting. Mounted Secret volumes are
  refreshed by the kubelet (after a short delay), while environment variables
  need a restart (for example with a tool like
  [Reloader](https://github.com/stakater/Reloader)). Connection pools should
  pick up the new password before opening new connections.

Dual-password overlap (two login roles alternating) is not supported.

The Cluster's own operator credentials (`<cluster>-credentials`) are not
rotated by this feature.
