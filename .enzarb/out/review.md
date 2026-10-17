Re-review of PR #57. Since 1f27fa8 only `.enzarb/out/*` artifacts changed, so the earlier conclusions hold and there are no must-fix problems. `go test ./internal/postgres` and `gofmt -l` pass on the PR head. I did not run the controller envtest suite, e2e or lint. CI runs e2e and lint.

Optional: don't merge `.enzarb/out/*`.
