## Review of PR #57

Implementation matches the plan. Guards run in Go against live server state, REASSIGN and DROP OWNED share one transaction per database, and the new path only runs when `deletion` is set. `go build`, `go vet ./internal/...`, `go test ./internal/postgres` and `go test ./internal/controller` (envtest, including the new CEL and `checkReassignTarget` tests) pass on the PR head. I did not run the e2e specs, lint or `make test-e2e`. CI runs lint and e2e (`.github/workflows/ci.yml`).

### Must-fix
None.

### Optional suggestions
- `.enzarb/out/plan.md` is committed in the PR. It is a tooling artifact and probably should not be merged.
- `REASSIGN OWNED` also moves tablespaces the role owns, and the guards only check for owned databases (`internal/controller/role_reassign.go:75`). This is an edge case, and arguably the intended outcome. Consider mentioning it in the docs, or refusing it as well.
- If a database fails to reassign, the error is logged and put in the skipped list. The message only reaches the user if `DROP ROLE` then fails with a dependency error. That is the intended behaviour, but a transient failure (for example a connection error) is surfaced indirectly.
- As the description says, there are no tests for the `RoleDropBlocked` condition path or for skipping a database that does not allow connections.
