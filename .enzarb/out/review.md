Re-review of PR #57. Since 0281a5a only `.enzarb/out/*` artifacts changed, so the earlier conclusions hold. I re-read `internal/postgres/role_reassign.go` and `internal/controller/role_reassign.go`. The guards run in Go against live server state. REASSIGN OWNED and DROP OWNED share one transaction per database, and identifiers go through `quoteIdent`. I found no must-fix problems.

Checks on the PR head:
- `go vet ./internal/... ./api/...` passed.
- `go test ./internal/postgres` passed.
- The controller envtest suite could not start here. The etcd binary is missing from the sandbox (`bin/k8s/.../etcd: no such file or directory`). CI runs it through `make test`.
- I did not run e2e or lint. CI runs both.

### Must-fix
None.

### Optional
- Don't merge `.enzarb/out/*`.
- `REASSIGN OWNED` also moves tablespaces the role owns, and the guards only check for owned databases. Consider documenting this or refusing it.
