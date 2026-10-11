<!-- Sync Impact Report: template → 1.0.0; five principles derived from AGENTS.md,
README.md, B01/A05 and the user's phase constraints. No template changes or deferred fields. -->
# GBaseLite Constitution

## Core Principles

### I. Accurate Compatibility and Documentation
Behavior, SQL, CLI, configuration, persistence, deployment, backup/restore and platform changes
MUST update README.md in the same change. Matrices, commands and limitations MUST match actual
behavior. Planned capabilities MUST be identified as planned. Complete MySQL/InnoDB compatibility
MUST NOT be claimed.

### II. Preserve Transaction and Storage Contracts
Changes MUST identify and test their impact on snapshot visibility, publication atomicity, root
retention, child isolation and commit validation. A05 parent-read/child-write and statement atomicity
MUST be preserved. Existing data MUST be preserved. Format or protocol changes require explicit
scope authorization and a separate compatibility plan.

### III. Backend Boundaries and Bounded Resources
SQL and physical operators MUST use storageengine contracts. Backend details belong in the adapter
and storage layer. Optional features MUST support capability detection and explicit unsupported
errors. Resources MUST have ownership, cleanup and enforceable bounds. A global transaction gate
MUST NOT substitute for row-level concurrency.

### IV. Verifiable Changes
Requirements MUST have acceptance criteria and traceable design, implementation and test tasks.
Concurrency tests MUST use deterministic synchronization rather than sleeps to establish ordering.
After Go changes, gofmt, go test ./... and go vet ./... MUST run. Tests MUST use isolated temporary
databases, clean them up and preserve data/.

### V. Authorized Phase and Deployment
Work MUST remain within the phase authorized by the user. Planning-only requests MUST NOT execute
implementation, change production code or deploy. Authorized deployments MUST build into .tmp,
then stop, replace bin/gbaselite, start and run the healthcheck.

## Additional Constraints
AGENTS.md remains the repository guidance. B01 and A05 describe established contracts; source
evidence takes precedence over historical status claims. New features MUST state explicit extensions
and limits without silently redefining existing APIs.

## Development Workflow and Review
Use installed Spec Kit skills and resolved templates. Review spec and clarification before planning;
finish research, model and contracts before tasks; analyze coverage before authorized implementation.
A passed document review is not evidence of passing runtime tests.

## Governance
The user's explicit instructions govern scope. Amendments MUST explain changed principles, record
compatibility consequences and use semantic versioning. Reviews MUST verify applicable principles.
This initial version records existing guidance rather than introducing new feature behavior.

**Version**: 1.0.0 | **Ratified**: 2026-10-10 | **Last Amended**: 2026-10-10
