# Changelog

All notable changes to GBaseLite are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Releases use numeric `major.minor.revision` versions, with non-zero revisions
padded to at least three digits, for example `1.0.001`; revision `999` rolls
over to revision 0 of the next minor version.

Before the first public 1.0.0 release, development snapshots used internal
build identifiers from 1.0.0 through 1.0.042. Their detailed notes are retained
below with an internal- prefix for traceability; they were not published
releases.

## [Unreleased]

### Added

- Added MySQL-compatible connection character sets `ascii`, `binary`, `latin1`,
  `utf8`/`utf8mb3`, and `utf8mb4`, together with their documented collations,
  handshake negotiation, session variables, `SET NAMES`, `SET CHARACTER SET`,
  `SHOW CHARACTER SET`, and `SHOW COLLATION` support.
- Added session-aware collation behavior for comparisons, `LIKE`, joins,
  ordering, `DISTINCT`, grouping, and window operations within the documented
  compatibility subset.
- Added MySQL-style `SET [SESSION] time_zone`, session/global timezone
  variables, `server.time_zone`, and `DB_TIME_ZONE`. `NOW()`,
  `CURRENT_TIMESTAMP`, `CURDATE()`, and timestamp defaults now use the current
  session timezone.

### Changed

- Named timezone data is embedded in the binary so IANA names work on hosts
  without a system timezone database. `DATETIME` and the current `TIMESTAMP`
  mapping remain wall-clock values rather than implementing MySQL's separate
  UTC-backed `TIMESTAMP` conversion.
- String storage and transport remain UTF-8. The added `ascii` and `latin1`
  modes provide protocol/session/comparison compatibility but do not transcode
  values to single-byte storage, and collations are not persisted per database,
  table, or column.
- Tag-triggered GitHub Actions builds now publish the same multi-architecture
  image and stable semantic tags to both GHCR and Docker Hub, using repository
  secrets for the Docker Hub login.

### Fixed

- Fixed `NOW()` and `CURRENT_TIMESTAMP()` rejecting a fractional-seconds
  precision argument such as `NOW(3)`; precision values from 0 through 6 are
  now supported.

- Release packaging now computes SHA-256 checksums through the .NET standard
  cryptography API when `Get-FileHash` is unavailable.

## [1.0.002] - 2026-08-05

### Added

- Added a dedicated remote publisher that creates an isolated
  release/v<VERSION> Git worktree, applies the explicit release version only in
  that branch, runs the existing complete packaging and validation workflow,
  verifies an ephemeral container, pushes multi-architecture Docker Hub tags,
  and atomically pushes the GitHub release branch and tag.

### Changed

- Bound official publishing to GitHub pucj0/gbaselite and Docker Hub
  pucj/gbaselite. Compose now pulls the Docker Hub image by default, while the
  GitHub Docker workflow publishes GHCR only to avoid duplicate Docker Hub
  pushes.
- Added exact TargetVersion support to the local one-click packager so a
  release branch packages the requested version instead of incrementing the
  development branch version.
- Standard-image Compose now tracks `pucj/gbaselite:latest`; release version
  synchronization leaves that file unchanged and continues updating the
  versioned external-binary path and release metadata.

### Fixed

- Docker containers now reclaim a persisted `gbaselite.pid` that names the
  current PID 1 process, so a bind-mounted data directory restarts cleanly
  after an ungraceful container stop without weakening live-process checks.
- Docker image startup now creates and repairs ownership of the data and log
  bind mounts before dropping privileges to UID/GID 10001. Startup still
  reports actionable diagnostics for read-only filesystems and SELinux label
  failures; the external-binary Compose remains fixed at UID/GID 65532.
- The Windows release publisher now keeps interactive command windows open
  after success or failure so PowerShell diagnostics remain visible, with a
  `--no-pause` option for automation.

## [1.0.0]

### Added

- First public release, consolidating the MySQL-compatible SQL subset,
  persistence safeguards, administration tooling, and cross-platform packaging
  developed through internal build 1.0.042.
- Release artifacts for Windows amd64, Linux amd64, and Linux arm64, plus
  multi-architecture container builds and versioned checksums.

### Fixed

- Docker multi-platform builds use Buildx-provided TARGETOS and TARGETARCH
  values, with post-push manifest platform validation.

## [internal-1.0.042] - 2026-08-03

### Added

- Added `ALTER TABLE ... DROP COLUMN [IF EXISTS]`, `RENAME COLUMN`, unnamed
  `ADD INDEX`, `ADD CONSTRAINT ... UNIQUE`, `RENAME INDEX`, and
  `ALTER COLUMN ... SET/DROP DEFAULT`. Comma-separated ALTER actions retain
  statement-level rollback when a later action fails.
- Added expression-based `UPDATE` assignments with MySQL left-to-right
  evaluation, automatic `ON UPDATE CURRENT_TIMESTAMP`, and atomic validation
  of unique, foreign-key, and CHECK constraints.
- Added row constructors, single- and multi-column `IN (SELECT ...)`, scalar
  subqueries, and `EXISTS/NOT EXISTS`. Correlated subqueries work in SELECT,
  UPDATE, and DELETE predicates and in UPDATE assignments; write evaluation
  uses stable snapshot row indexes and retains statement-level rollback.
- Added top-level chained `UNION`, `UNION DISTINCT`, and `UNION ALL` with final
  `ORDER BY` and `LIMIT/OFFSET` processing. UNION queries can also be used as
  derived tables, views, `INSERT ... SELECT`, scalar/IN/EXISTS subqueries, and
  `EXPLAIN` input.
- Added MySQL `INSERT ... SET`, singular `VALUE`, optional `INTO`, expression
  values, and prepared-protocol coverage for that write form.
- Added ordered non-recursive multi-CTE queries, including references from a
  later CTE to an earlier CTE.
- Added atomic `CREATE TABLE ... AS SELECT` and `CREATE TABLE ... LIKE`.
  CTAS infers columns from query metadata; LIKE copies columns, defaults,
  indexes, CHECK constraints, and the table comment, but not foreign keys.
- Added MySQL `REPLACE` for VALUES/VALUE/SET/SELECT input. All rows conflicting
  on any PRIMARY/UNIQUE key are deleted before insertion, and affected rows are
  reported as deleted rows plus inserted rows.
- Added `UPDATE ... JOIN`, `DELETE target,... FROM ...`, and
  `DELETE FROM target,... USING ...`. Joined writes use stable internal row
  indexes, deduplicate repeated join matches, and commit the statement from an
  isolated store clone.
- Added same-database `ON DELETE/UPDATE CASCADE` and `SET NULL` foreign-key
  actions, including multi-level and cyclic cascade protection. Cascaded rows
  are validated together with explicit changes before commit.

### Fixed

- Added `SHOW FULL COLUMNS ... LIKE/WHERE` filtering and fixed
  `information_schema.COLUMNS` filtering by `COLUMN_NAME`.
- Fixed unspaced subtraction such as `balance-100` being tokenized as a
  negative number instead of a binary expression.
- Converted numeric `ON DUPLICATE KEY UPDATE` expression results through the
  destination column's normal coercion path.

## [internal-1.0.041] - 2026-08-03

### Changed

- Automated release build.

## [internal-1.0.040] - 2026-08-03

### Fixed

- Fixed ordinary-table `ORDER BY` execution for boolean predicate expressions
  such as `owner_user_id IS NULL`, `visibility = 'global'`, and
  `revoked_at IS NULL`, as well as `CASE WHEN ... THEN ... ELSE ... END` sort
  keys. Pure column ordering keeps the existing index path.

## [internal-1.0.039] - 2026-08-02

### Added

- Added atomic MySQL-compatible comma-separated `ALTER TABLE` actions for the
  supported column, index, constraint, and table-comment operations. If any
  action fails, none of the preceding actions from that statement are kept.

## [internal-1.0.038] - 2026-08-02

### Added

- Added MySQL-compatible `ALTER TABLE ... ADD COLUMN`, named
  `ADD/DROP FOREIGN KEY`, named `ADD/DROP CHECK`, same-database atomic
  `RENAME TABLE`, and basic `EXPLAIN SELECT` support. Constraint additions scan
  existing rows before changing metadata.
- Added ordered in-memory access paths for ordinary and composite indexes.
  Simple base-table queries now use left-prefix equality, a range on the next
  index column, matching ascending/descending order, early LIMIT/OFFSET, and
  indexed COUNT paths.

### Fixed

- Enforced parent-row DELETE/TRUNCATE `RESTRICT` for same-database foreign keys
  and mapped the protocol error to MySQL 1451. Explicit unsupported cascading
  actions now fail instead of silently behaving as another action.
- Compared BOOLEAN columns with prepared binary-protocol bool and integer
  parameters using MySQL numeric semantics, including equality, IN, and
  UPDATE/DELETE predicates.
- Preserved names and referential actions for foreign keys and CHECK constraints
  in `SHOW CREATE TABLE`, logical backups, and compatibility
  `information_schema` metadata.

### Changed

- Advanced the database snapshot format from version 2 to version 3. The
  ordered migration assigns stable names to legacy foreign keys and converts
  legacy CHECK expressions into named constraint metadata without changing
  business rows.

## [internal-1.0.037] - 2026-08-01

### Changed

- Automated release build.

## [internal-1.0.036] - 2026-08-01

### Fixed

- Kept DATETIME defaults on the server's local wall clock so
  `DEFAULT CURRENT_TIMESTAMP` and `NOW()` agree, and accepted the
  `CURRENT_TIMESTAMP` keyword without parentheses.
- Returned aggregate `COUNT(*)` projections from compatibility
  `information_schema` queries, including one row containing zero when no
  metadata rows match.
- Added temporal `+` and `-` arithmetic with MySQL `INTERVAL` operands.
- Preserved consumed auto-increment IDs across transaction rollback, ordinary
  DELETE, persistence, and restart; TRUNCATE continues to reset the sequence.

### Changed

- Advanced the database snapshot format from version 1 to version 2 to persist
  each table's next auto-increment value. The ordered migration initializes it
  from existing rows without changing schemas or row data.

## [internal-1.0.035] - 2026-08-01

### Fixed

- INSERT OK packets now expose generated auto-increment IDs, and
  `LAST_INSERT_ID()` reports the connection's latest generated value.
- Enforced inline and named unique keys, same-database foreign keys, CHECK,
  ENUM, and JSON validation for new schemas, including transactional writes.
- Added arithmetic, CASE, correlated scalar subqueries, MySQL upserts, nested
  aggregate expressions, and function expressions in GROUP BY.

## [internal-1.0.034] - 2026-07-31

### Added

- Added explicit database snapshot and user-catalog format versions with ordered
  migration registries. Existing unversioned gob files are read as legacy
  version 0, unknown newer versions fail closed, and the inspection commands
  report both effective and source versions without rewriting the files.
- Added `gbaselite inspect-snapshot --file ... [--compare ...]` for read-only
  decoding and structural comparison of copied database snapshots and recovery
  candidates. It reports file metadata, SHA-256, and aggregate
  database/table/index/view/row counts without printing object names, SQL, or
  row values, and never modifies or promotes either file.
- Added CI coverage that runs the complete Go test/vet suite on both Ubuntu and
  Windows and uses the official MySQL 8.4 client and `mysqldump` against an
  isolated temporary GBaseLite instance. The smoke workflow verifies hyphenated
  database discovery, table/view/index export, executable-comment view import,
  restored data, and cleanup without using deployment data.
- Added repeatable benchmarks for 10,000-row range queries, 100-row persistent
  insert batches and 1,000-row equality joins, complementing the existing point
  lookup, persistent update, streaming, rollback and multi-connection MySQL
  protocol benchmarks.
- Added an abrupt-process-exit recovery regression for acknowledged writes and a
  deterministic 250-operation insert/update/delete sequence that reopens storage
  every 25 operations and compares all persisted rows with an independent model.
- Extended `gbaselite diagnose` with cross-platform data/log volume totals and
  available bytes, the active main-log size, rotated-log count and bytes, and
  audit/binlog file status even when those journals are disabled. The read-only
  report never scans row contents or log contents.
- Added `replay-binlog --check-only` to validate JSONL decoding, format
  versions, increasing sequences, and replay filters without executing SQL or
  opening a data directory. An explicit `--input` can be checked without loading
  a configuration file or stopping a running service.
- Added `inspect-instance --directory ...` for read-only validation of a stopped
  data-directory copy. It decodes both the database snapshot and user catalog,
  reports aggregate database/table/ index/view/row/account/grant counts without
  exposing names, SQL, credentials, password hashes, or row values, and rejects
  copies that contain unresolved snapshot or user-catalog recovery candidates.
- Snapshot inspection summaries now include persisted primary, unique, and
  ordinary index counts.

### Fixed

- Simplified the binary Docker Compose environment example: it uses no Compose
  `${...}` interpolation, keeps only the container-external listener active, and
  documents security, TLS, log, audit, and binlog overrides as commented
  opt-ins. The password is configured directly in the Compose file.
- Navicat parallel data transfer no longer fails when a decorated `CREATE VIEW`
  reaches the target just before its dependent table. Only missing-dependency
  validation is deferred for views carrying MySQL transfer options; plain views,
  invalid columns, and circular definitions remain fail-fast.
- Clarified that renamed copy placeholders are never persisted or returned by
  metadata queries, while Navicat 17 may retain its own optimistic placeholder
  in the tree until the user refreshes it.
- MySQL 8 `mysqldump` table-data reads using
  `SELECT /*!40001 SQL_NO_CACHE */ * FROM ...` are now parsed as ordinary
  selects. Common MySQL select modifiers are accepted and ignored where they are
  optimizer-only, allowing database dumps to include and restore table rows.
- Navicat dump imports now classify `SET NAMES` and `SET FOREIGN_KEY_CHECKS`
  correctly when the client sends the dump header block comment or line comments
  in the same protocol request. Leading ordinary comments no longer route these
  session settings to `SET PASSWORD` parsing.

## [internal-1.0.033] - 2026-07-31

### Fixed

- MySQL executable comments such as `/*!50001 CREATE VIEW ... */` are now
  unwrapped consistently by protocol execution, direct execution, and offline
  restore. MySQL 8 dump imports now restore view definitions instead of silently
  treating them as ordinary comments, and audit entries record the actual
  operation with redacted expanded SQL.

## [internal-1.0.032] - 2026-07-31

### Fixed

- Plain `SHOW TABLES` now follows MySQL behavior and includes both base tables
  and views, allowing MySQL 8 `mysqldump` and clients using the same enumeration
  path to export view definitions. `SHOW TABLE STATUS` now returns MySQL-style
  view rows, and `SHOW CREATE DATABASE IF NOT EXISTS` is accepted and returns
  replayable quoted DDL.

## [internal-1.0.031] - 2026-07-31

### Added

- `gbaselite diagnose --config ...` now performs a read-only local diagnostic of
  the configured listener, data/log directories, durable snapshot and user
  catalog, TLS certificate loading, and audit/binlog settings without printing
  credentials or decoding live data files. Missing critical paths, invalid TLS
  material, unreachable listeners, and recovery candidates produce a non-zero
  exit.
- MySQL `SHOW STATUS`, `SHOW GLOBAL STATUS`, and `SHOW SESSION STATUS ... LIKE`
  now expose real process-lifetime connection, query, aborted-connect, uptime,
  and TLS metrics. Session TLS status reports the negotiated protocol version
  and cipher suite.
- The primary `gbaselite.log` now rotates before the next write would exceed a
  configurable size (20 MiB by default) and prunes timestamped rotated files
  after a configurable retention period. Retention defaults to 7 days, accepts
  1-365 days, and `0` keeps rotated logs permanently.

### Changed

- YAML, environment, Docker, diagnostics, and MSI configuration preservation now
  include `log.max_size_mb` and `log.retention_days`. Cleanup targets only
  timestamped primary-log rotations; it never removes the current log, audit
  JSONL, binlog, or unrelated files.

### Security

- Runtime status metrics retain the existing fail-closed contract: once
  persistence fails, status SQL cannot bypass MySQL error 1030. Diagnostic
  output excludes usernames, passwords, password hashes, SQL text, and stored
  row contents.

## [internal-1.0.030] - 2026-07-30

### Added

- Optional MySQL-protocol TLS now supports TLS 1.2 and newer with configured PEM
  certificate and key files. `tls.require_secure_transport` can reject plaintext
  authentication with MySQL error 3159, while TLS remains disabled by default
  for existing clients.
- TLS configuration can be supplied through YAML or `DB_TLS_*` environment
  variables. Startup rejects missing, unreadable, or mismatched certificate
  material before opening the TCP listener.

### Changed

- MSI configuration rewrites preserve an existing TLS section without adding
  certificate choices to the installer UI. Docker examples expose the optional
  TLS environment settings; certificate files must be mounted separately and
  kept outside release archives.

## [internal-1.0.029] - 2026-07-30

### Fixed

- Windows database snapshots, user catalogs, and logical backup outputs now
  replace their previous files atomically without deleting the last good copy
  first. Replacement failures preserve the prior durable file.
- Startup now refuses to create an empty store or bootstrap account when a crash
  recovery candidate such as `store.gob.tmp` or `users.gob.tmp` exists, and
  corrupt gob files report their path plus explicit copy-first backup/recovery
  guidance instead of silently falling back to an empty instance.
- A failed database snapshot now puts the engine into a fail-closed state, fails
  every pending group commit and later SQL request with MySQL error 1030, and
  prevents shutdown from retrying divergent in-memory state over the last
  durable snapshot. Restart reloads only the last successful snapshot.

### Changed

- Transaction documentation now states the implemented isolation contract: an
  explicit transaction holds an instance-wide exclusive gate, reads its own
  snapshot, and blocks other sessions' reads and writes until commit, rollback,
  or disconnect rollback. A concurrent regression covers all three release
  paths.

## [internal-1.0.028] - 2026-07-30

### Added

- Configurable authentication failure throttling now tracks each source IP and
  username, returns the same generic MySQL 1045 response while blocked, and
  emits password-free structured audit events. Defaults are five failures in 60
  seconds followed by a 30-second block; setting any throttle value to zero
  disables it.
- Detached `start` now returns the fresh child-process startup diagnostic and
  records initialization failures in `gbaselite.log`, including missing
  bootstrap passwords, instead of pointing users to an empty log.

### Security

- New non-container configurations now listen on `127.0.0.1` by default. MSI
  upgrades preserve an existing `server.host`, while new installations use
  loopback and restrict configuration, data, and log ACLs to `SYSTEM` and local
  administrators.
- The checked-in development configuration no longer contains the weak sample
  password `123456`.
- The primary service log is created and tightened to owner-only `0600`
  permissions on platforms that implement POSIX modes.
- Existing user catalogs can start with an empty bootstrap `auth.password`; a
  password is required only when `users/users.gob` does not yet exist. Existing
  catalogs no longer recreate a deliberately removed bootstrap account from
  configuration, and no password hash is changed or derived.

## [internal-1.0.027] - 2026-07-30

### Fixed

- Navicat grid row deletion now accepts MySQL-style
  `DELETE ... WHERE ... LIMIT n` and reports the actual affected row count
  without deleting later matching rows beyond the limit.
- A real MySQL-protocol regression now covers Navicat database/table creation,
  grid updates and deletes, table/view copy naming, placeholder invisibility,
  GBaseLite-to-GBaseLite transfer name preservation, and prepared dump
  enumeration in one pinned client session.

## [internal-1.0.026] - 2026-07-30

### Fixed

- Navicat database dumps can enumerate hyphenated databases through prepared
  `SHOW FULL TABLES` and `information_schema` metadata queries without fake
  system schemas obscuring the selected persistent database.
- Named `UNIQUE KEY` and `KEY` definitions inside `CREATE TABLE` are now
  validated, persisted, and included in `SHOW CREATE TABLE` and logical backups
  instead of being silently discarded. Invalid inline index columns fail
  atomically without leaving a partial table.

## [internal-1.0.025] - 2026-07-30

### Added

- Audit and logical binlog retention can be configured independently from `0` to
  `365` days, defaulting to `7`; `0` keeps records permanently. Startup and
  daily compaction keep the JSONL filenames stable, while MSI and environment
  settings expose the same validated range.

## [internal-1.0.024] - 2026-07-30

### Added

- Windows MSI now provides a separate audit and recovery logging page with
  independent checkboxes for structured audit logging and replayable logical
  binlog. Change and upgrade flows preload the existing enablement state without
  replacing custom paths.

## [internal-1.0.023] - 2026-07-30

### Added

- Optional JSONL audit logging for authentication and SQL operations, including
  connection ID, authenticated account, remote IP, database, result, affected
  rows, duration, and redacted SQL.
- Optional transaction-aware logical binlog with ordered JSONL records and the
  offline `replay-binlog` command. Rolled-back and failed changes are excluded.

### Fixed

- Windows services now write `gbaselite.log` before attempting console output,
  so an unavailable service stdout handle no longer prevents file logging.
- Windows MSI configuration updates preserve existing audit/binlog enablement
  and paths; new installations write both sections explicitly with safe disabled
  defaults.
- Windows MSI reinstalls restore the persisted installation directory even when
  the prior program files have already been removed.
- Docker release examples no longer use `123456` as the sample administrator
  password.

## [internal-1.0.022] - 2026-07-29

### Fixed

- `SHOW DATABASES` and `SHOW SCHEMAS` no longer advertise the non-selectable
  compatibility placeholders `information_schema` and `mysql`; results now
  contain only accessible persistent databases, allowing Navicat database-level
  SQL dumps to enumerate the selected business tables instead of producing a
  header-only file.

## [internal-1.0.021] - 2026-07-29

### Changed

- Automated release build.

## [internal-1.0.020] - 2026-07-29

### Changed

- Automated release build.

## [internal-1.0.019] - 2026-07-29

### Fixed

- Windows MSI no longer waits up to 30 seconds for the optional post-install
  service startup. A port conflict or startup failure now leaves the service
  stopped without holding the installer UI.
- Windows MSI Change and upgrade flows now apply the port selected on the
  configuration page while preserving an existing administrator password,
  instead of silently retaining the previous port.
- Windows MSI now validates the selected port before continuing. It blocks ports
  held by other programs while allowing an existing GBaseLite listener to be
  replaced during installation.

## [internal-1.0.018] - 2026-07-29

### Changed

- Automated release build.

## [internal-1.0.017] - 2026-07-29

### Fixed

- Windows MSI reinitialization now retains the prior data directory as an
  adjacent backup instead of recursively deleting it during installation, so
  large data directories no longer hold the installer at the backup-file cleanup
  stage.

## [internal-1.0.016] - 2026-07-29

### Changed

- Windows MSI no longer installs the inactive `config.example.yaml` beside
  `gbaselite.exe`. Its only service configuration remains
  `%ProgramData%\GBaseLite\config.yaml`; portable ZIP packages continue to
  include the example file.

## [internal-1.0.015] - 2026-07-29

### Changed

- Automated release build.

## [internal-1.0.014] - 2026-07-29

### Changed

- Automated release build.

## [internal-1.0.013] - 2026-07-29

### Changed

- Navicat table and view copies now use `source_copy_YYMMDDNN`, where `NN` is
  the source object's two-digit daily sequence beginning at `01`; minute
  timestamps and `_2` / `_3` collision suffixes are no longer generated.

### Fixed

- `SHOW TABLE STATUS ... LIKE` now filters the result instead of returning every
  table, preventing Navicat data-transfer workers from mapping different
  selected sources to the destination's last table and racing to drop or create
  that same object.
- Navicat's `_copy` and `_copyN` placeholders are never stored or exposed by
  relation listings. Renamed CREATE responses include the final server name, set
  the metadata-changed status, and map same-session metadata and insert requests
  directly to that final object.
- Navicat GBaseLite-to-GBaseLite transfers that inspect, drop, and recreate
  destination tables or views now clear copy-only session state, preserving
  every original object name during parallel transfer instead of misclassifying
  the replacement as a timestamped local copy.

## [internal-1.0.012] - 2026-07-29

### Fixed

- Windows MSI maintenance now enables both Change and Repair. Change reopens the
  database configuration page and can add or remove the optional desktop
  shortcut; Repair restores installed program resources while retaining
  configuration and data.

## [internal-1.0.011] - 2026-07-29

### Changed

- Automated release build.

## [internal-1.0.010] - 2026-07-29

### Added

- The Windows MSI now embeds a multi-size GBaseLite application icon, installs a
  branded Start menu shortcut, and offers an unchecked-by-default desktop
  shortcut option.

### Changed

- The MSI license page now displays a readable Chinese license agreement, and
  standard/custom installer dialogs share branded welcome artwork, banners,
  typography, and warning hierarchy.

## [internal-1.0.009] - 2026-07-29

### Fixed

- Windows CLI and release scripts now initialize the console as UTF-8 so Chinese
  identifiers, query results, and .NET/WiX build messages are displayed
  correctly in CMD and Windows Terminal.
- The built-in client now renders aligned MySQL-style tables, shows the active
  database in its prompt, formats protocol errors cleanly, reports elapsed time
  for every SQL statement, and accepts UTF-8 BOM-prefixed input from Windows
  PowerShell pipelines.
- `INSERT IGNORE` now skips primary-key and unique-index conflicts instead of
  behaving like a plain `INSERT`, allowing repeat-safe data-transfer modes
  without weakening normal duplicate-key errors.

## [internal-1.0.008] - 2026-07-29

### Changed

- Automated release build.

## [internal-1.0.007] - 2026-07-29

### Fixed

- MSI configuration now leaves data reinitialization unchecked by default and
  restores the prior 64-bit registered installation, data, and log directories
  during upgrade or reinstall.

## [internal-1.0.006] - 2026-07-29

### Changed

- Automated release build.

## [internal-1.0.005] - 2026-07-29

### Changed

- Release versions now appear only in the `dist/GBaseLite-<VERSION>` directory
  name; MSI, ZIP, tarball, and raw binary filenames inside the directory no
  longer repeat the version.

## [internal-1.0.004] - 2026-07-29

### Changed

- One-click release versions now roll from `1.0.999` to `1.1.0`, then continue
  at `1.1.001`.

## [internal-1.0.003] - 2026-07-29

### Fixed

- One-click release tool bootstrapping no longer mixes .NET, WiX, or extension
  installer output into executable path return values when `.tmp` has been
  cleared.

## [internal-1.0.002] - 2026-07-29

### Changed

- Automated release build.

## [internal-1.0.001] - 2026-07-29

### Changed

- One-click releases now publish every version into its own
  `dist/GBaseLite-<VERSION>` directory, with all bundled release Skills
  synchronized to the same Docker and output layout.
- MSI major upgrades now show the configuration page, restore the existing data
  directory, and keep reinitialization disabled unless the user checks it and
  completes the second confirmation page.

## [internal-1.0.0] - 2026-07-29

### Added

- Root-level `release.bat` one-click releases with automatic padded version
  increments, rollback on failure, portable tool bootstrapping, Compose and
  candidate validation, and publication to `dist`.
- MySQL-style `gbaselite -u root -p` TCP client login with host, port, database,
  and execute options.
- Cross-platform Windows amd64, Linux amd64, and Linux arm64 release builds.
- WiX-based Windows MSI source with service registration and data retention.
- Published-image, local-build, and external-binary Docker Compose variants.
- GitHub Actions workflows for tests, releases, and multi-architecture images.
- Direct Alpine external-binary Compose deployment without building an
  application image.
- Raw Linux deployment binaries are emitted into `dist` alongside release
  archives.
- Windows MSI controls for automatic startup and post-install service startup
  are independent.
- The Windows MSI data directory has a `%ProgramData%\GBaseLite\data` default
  and a system folder browser.
- External-binary Compose now declares every `DB_*` override explicitly under
  `environment`.

### Fixed

- Renamed Navicat table and view copies now set MySQL's metadata-changed server
  status so clients can refresh server-selected copy names immediately.
- Windows MSI now installs the GBaseLite directory into the machine `Path`
  through a dedicated, validated component so new terminal sessions can invoke
  `gbaselite` directly.
- Navicat table and view copies now keep the source object and persist each copy
  under a collision-safe server-selected name.
- Windows MSI packages now use Simplified Chinese for standard and custom
  installer dialogs.
- Windows portable start, stop, and restart batch files now resolve both source
  and ZIP layouts and keep double-clicked windows open so errors remain visible.
- Windows MSI installation no longer rolls back solely because an existing
  process prevents the newly installed service from starting.
- MSI default configuration and data paths are resolved after directory costing
  so the configuration page is populated correctly.
- Windows MSI now stops an existing `GBaseLite` Windows service and safely
  identifies a standalone `gbaselite` listener by the selected port and process
  name before service startup.
- External-binary Compose port publishing and health checks now follow `DB_PORT`
  , and startup requires an explicit `DB_PASSWORD`.
- External-binary Compose no longer bind-mounts an optional config path that
  Docker could silently create as a directory when the source file is absent.
- Windows MSI now packages a CLR v4 activation configuration for WiX DTF custom
  actions, preventing error `0x80131700` and installation rollback on systems
  without CLR v2 activation.

## [0.9.0] - 2026-07-28

### Added

- Initial open-source preview of the MySQL-compatible GBaseLite server.
