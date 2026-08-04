# Repository Instructions

- Every change to behavior, SQL compatibility, CLI commands, configuration,
  persistence, deployment, backup/restore, or supported platforms must update
  `README.md` in the same change.
- Keep the feature matrix, deployment commands, and limitations accurate. Never
  claim complete MySQL compatibility.
- Preserve existing data under `data/`. Use isolated temporary databases for
  write verification and remove them after testing.
- Run `gofmt`, `go test ./...`, and `go vet ./...` after Go code changes.
- Build deployments into `.tmp` first, then stop, replace `bin/gbaselite`,
  start, and run the healthcheck.
