# Instructions for code agents

The `user` service of «Понятные финансы» (B2C personal finance: OFX statement import from T-Bank, operations,
analytics, LLM assistant): phone sign-in confirmed by a callback call, registration, access and refresh tokens,
profile, consents and their documents, account deletion, JWT signing keys for the other services; it starts
`PurgeUserData` and `SetUserTimezone` in other services through the outbox. Contract: `finassist.user.v1` in
`fin-assist/contracts` (`docs/api.md` 1–2, `docs/grpc-api.md` 7.1). Made from `fin-assist/go-service-template`
(`main` 740a202, FIN-191). Read by Claude Code (through `CLAUDE.md`) and by Codex. This file is the orientation map:
read it and `README.md` first, then open only what the task touches.

## Keep this file current

A PR that changes what this file, `README.md` or `docs/` say (layout, commands, CI checks, conventions, process)
updates them in the same PR. A service made from the template rewrites the first paragraph for itself.
- Blocks between `<!-- generated:… -->` markers are produced from code; do not edit them by hand. The tables of
  `docs/config.md` come from `Vars` in `internal/config/vars.go`: a new variable or default is a line there, then
  `go test ./internal/config -run TestUnitDocIsCurrent -update`. `go test` fails on a stale table, on a variable read
  but not declared, and on one declared but never read.

## Where things are decided

- Contracts: `fin-assist/contracts` — `docs/api.md` (REST), `docs/grpc-api.md` (services, call models, metadata,
  errors, idempotency, outbox, transport), `docs/calc.md` (calculations), `proto/`. The service imports the generated
  module `github.com/fin-assist/contracts/gen/go` (messages, gRPC, `finerr`) at a tag `gen/go/vX.Y.Z`. A service
  never defines its own `.proto`, error codes or REST fields: a change goes to `contracts` first (its own FIN issue
  and PR), then the service bumps the module.
- Shared code: Go libraries made from `fin-assist/go-library-template` (FIN-118) — `go-identity`, `go-idempotency`,
  `go-outbox`, `go-tombstone`, `go-postgres` (v0.3.0, FIN-253), `go-grpc` (v0.2.0, FIN-250), `go-worker` (v0.1.0, FIN-252), `go-migrate` (v0.2.0, FIN-251/262). The service imports them at tagged versions and wires them in `internal/app` by the
  «Внедрение» section of their README; versions and compatibility — `docs/releasing.md` of the library template.
  Code that more than one service needs goes into a library (its own FIN issue), not into a copy here.
- Architecture, MVP scope, product decisions: Confluence PF (https://finassist.atlassian.net/wiki/spaces/PF),
  «Архитектура» (including «Миграции и доступ к БД» and «Репозитории и структура кода»). When the docs here and the
  contracts differ, the contracts and the owner's latest decision win; say so in the PR.
- Every change goes through a YouTrack issue (`FIN-<n>`, finassist.youtrack.cloud) referenced in the PR.
- Mismatches between the contracts, Confluence and this repository are expected. Resolve them in favour of the
  contracts and the owner's latest decision, say so in the PR, and propose (not impose) engineering changes; keep them
  non-radical. The owner writes backend code himself and is strong in PostgreSQL: schemas and queries are proposed
  with arguments, not decided for him.

FIN-255 uses go-grpc v0.2.0 server/client/reqctx, go-migrate v0.2.0 and go-worker v0.1.0 directly; no local transport/context/runner copies. go-postgres v0.3.0 owns pool lifecycle; routing and business transaction mapping remain service-owned. Workers receive the logger, SystemClock and explicit five-second restart delay. Shared tests belong to the libraries; service identity, ACL, SQL and RPC scenarios remain here.

FIN-225 uses contracts/gen/go v0.5.0. Usecase-owned `domain.WithRequestRetry` marks only explicit refusal phases; `adapter/grpc/retryinfo` serializes that decision after mapping, and `adapter/idempotency.StartEngine` preserves callback advice before encoding. Never infer permission from a reason/status or decorate an old decoded cache. Positive V1 is restricted to the initial pre-effect limiter; unknown storage/COMMIT/provider outcomes stay unmarked. See `docs/errors.md`; this does not change keys, TTL or provider behavior.

## Current implementation (FIN-219)

Functional RPCs: GetConsentDocument, GetSigningKeys, GetAssistantConsentStatus, GetProfile,
UpdateProfile, GrantAssistantConsent, RevokeAssistantConsent, Register, RefreshTokens,
Logout, DeleteAccount, WatchPhoneVerification and StartPhoneVerification (all 13 RPCs).
The default executable disables phone creation. FIN-139/216 adds opt-in Zvonok V1
for dev/stage, a transactional uniform number selector, a separate checked postback
listener and a Watch fallback. Zvonok prod remains blocked until FIN-81.
FIN-122 adds PHONE_PROVIDER=fake in dev/stage, restricted to synthetic +7000000xxxx.
Creation completion, shared selector, common Confirm and replay commit atomically;
fake recovery is stateless. Prod startup forbids fake; common Confirm rejects the
synthetic namespace for every provider and emits a private-label-free counter/ERROR.
The alert rule is prepared in deploy/prometheus; deployment activates it separately.
Balance monitoring is deferred to FIN-233 and does not block FIN-139/216.
Real campaign acceptance still awaits external evidence.
Do not claim production phone sign-in is ready. go-identity v0.2.0 `NewAssembly` (FIN-325)
supplies the verifiers, outgoing token and one `identity-keys` refresh job: ENV=prod is Kubernetes,
ENV=stage file keys, verify in dev is refused; standard AUTH_* variables, renamed old names fail
startup. Keep the existing server ACL and reqctx chain, without a second identity interceptor.
The signing package supplies its local JWKS provider as `UserKeys` (no self-RPC); incoming user
tokens use JWT_ISSUER/JWT_AUDIENCE. go-tombstone
v0.1.0 reads primary and its migration source precedes the service. Assistant documents check
the conditional user token and tombstone in their handler; personal-data documents are public
to authenticated bank-bff. Signing key files are required even in insecure-dev. Make up/run
create an ignored .local key through `cmd/dev-key` (not shipped in the service image); never put that directory in git or the Docker build context.

Profile writes take the tombstone guard and the user row lock in one primary transaction.
Assistant consent is effective only for the current document version; Status is primary-only,
assistant-only SYSTEM per grpc-api.md 3.3, despite the mixed-model comment in the proto.
go-outbox v0.2.0 publishes timezone changes through `NewPublisher(a.DB, time.Now)` in the
caller-owned transaction; Publish requires its callback context and does not commit.
The dispatcher runs as `worker.Job{Name: "outbox", Run: dispatcher.Run}` with a service
lifecycle context, never a go-postgres transaction context (`postgres.ErrForeignTransaction`).
Its internal Manager uses a pinned connection from a dedicated primary pool; selection
commits before RPC, and the session advisory lock survives through acknowledgement.
Delivery uses operator-configured recipients and system credentials only.
Timezone defaults to bank-statement-data and assistant; deletion requires bank-statement-data,
analytics and assistant. Pool capacity is at least the union of recipients + 3. Missing configured peer addresses fail startup. See docs/profile.md.

Start, Register and RefreshTokens use go-idempotency v0.6.0 with encrypted no-user replies
and result ownership (FIN-226). Start also binds reservations and cached replies,
including busy refusals, to the verification request through Resource (FIN-230).
Their key/effect transactions use a dedicated primary pool; IP attempts commit in
the ordinary pool before token checks, even for rejected secrets. Reuse rolls the
non-cacheable 401 back, then rechecks and commits revocation before returning it.
Response.OwnerID binds cached secrets to the actual account in idempotency_keys:
replay requires an owner and takes its tombstone guard; deletion purges scope OR owner_id.
Two distinct confirmed registration tokens for one phone create separate sessions
without overwriting an existing account (owner decision, FIN-219). Logout has its own
exact ModelUser rule (grpc-api.md 3.3) and deletes only the token owner's chain (FIN-285).
See docs/auth.md.

DeleteAccount uses the exact ModelUser AllowDeleted rule so a verified old access token
can repeat deletion; other calls still reject tombstoned identities. Purge atomically removes
local data, owned/scoped cached credentials, every request of the phone and old timezone
jobs, then schedules user.deleted. Repeats add no jobs. Cached-key locks use NOWAIT:
a busy replay rolls back the whole deletion before retrying, bounded by the caller and
10 seconds. Never wait for an auth key while retaining the tombstone guard. See docs/deletion.md.

Start commits the library reservation, four rate-limit slots and phone claim before
calling Provider.Create/Recover outside a transaction. Its durable phone_verification.start_*
fields own recovery and refundable windows. Library Resource binds the own reservation
and successful/busy caches to the request ID; busy keys never overwrite attempt fields. Completion takes key -> guard/user -> request;
fresh claims take key -> limits -> guard/user -> request -> BindResource. Selector runs inside
the completion transaction. Only ReleaseIfCurrent permits refund after a definite provider
rejection, in the same transaction as release; uncertain outcomes keep their claim.
Deletion also purges successful and busy Start caches through phone links, including
keys created before the account existed. Cleanup retains requests while a Resource cache is live. Purge locks request parents
before its NOWAIT key snapshot; busy claims take KEY SHARE to close the uncommitted-link race.
Runtime backend selection is PHONE_PROVIDER; app.WithPhoneBackend and WithZvonokTransport
are trusted assembly seams for API tests. The endpoint stays pinned to HTTPS Zvonok V1.

Phone confirmation accepts provider-checked Proof through the trusted Go usecase only.
The Zvonok ingress handler accepts only the fixed provider IP (forwarding headers only
from configured trusted proxies), resolves a local call ID, then checks its status/phone
with the provider API. Postback phone/status hints never constitute proof. Watch commits an IP attempt before secret lookup. A shared
primary LISTEN connection serves bounded pending subscriptions; subscribe then re-read closes
the snapshot/notification gap. Ciphertext uses a separate AES-256 key and binds request ID
plus verification-token hash. Existing-user confirmation takes Guard -> user -> request;
result reads retry if ownership changes before the shared request lock. No stream retains
its transaction. Results live 15 minutes from confirmation, independent of request expiry.
Cleanup commits request-only wiping/release, session-only detachment and unreferenced old
request deletion separately: never retain both session/request row locks in reverse order.
See docs/verification.md, including capacity/availability metrics and key retention.

## Repository map

| What | Where |
|---|---|
| Commands `serve`, `migrate`: listeners, shutdown | `cmd/user/` (`serve.go`, `migrate.go`) |
| Assembly: identity, pools, server; wiring of adapters, usecases and handlers (`wire()`), the access table (`AccessPolicy()`), migration sources | `internal/app/` (`app.go`, `wire.go`) |
| The whole service in-process for API tests | `internal/app/apitest/` |
| Access JWT signing, rotation snapshot, local public JWKS | `internal/signing/` |
| Opening secret files with the owner-only check (systemd credentials 0400/0440) | `internal/secretfile/` |
| Opaque 256-bit secrets, shared IANA timezone validation | `internal/token/`, `internal/domain/` |
| Registration, refresh rotation/reuse and logout | `internal/auth/`, `internal/adapter/grpc/auth/`, `internal/adapter/postgres/auth.go` |
| Phone creation/recovery, confirmation, snapshots, Watch and notification fanout | `internal/verification/`, `internal/adapter/verificationcrypto/`, `internal/adapter/postgres/verification*.go`, `internal/app/phone.go`, `internal/app/sessions.go` |
| Current consent-document usecase | `internal/consent/` |
| Profile settings, consent lifecycle and system status | `internal/profile/`, `internal/adapter/grpc/profile/`, `internal/adapter/postgres/profile.go` |
| Atomic deletion and local data purge | `internal/deletion/`, `internal/adapter/postgres/deletion.go` |
| Timezone/deletion events and dispatcher assembly | `internal/adapter/outbox/`, `internal/app/outbox.go` |
| Entities and invariants (no pgx, gRPC or generated code) | `internal/domain/` |
| Usecases and the interfaces of repositories and clients they need | `internal/<feature>/` |
| gRPC server, interceptors, access table types, identity and tombstone interfaces | `github.com/fin-assist/go-grpc/server`; rules — `docs/server.md` |
| gRPC handlers (messages ↔ usecases) | `internal/adapter/grpc/<feature>/` |
| Pools, transaction manager, repositories | `internal/adapter/postgres/`; rules — `docs/migrations.md`, «Запросы на pgx» |
| Migration runner and the PostgreSQL for tests | `github.com/fin-assist/go-migrate`, public `go-migrate/migratetest`, local server selection `internal/testdb/` |
| SQL migrations (embedded) and the tests of their keys, cascades and expected queries | `migrations/`; rules — `docs/migrations.md`; the schema, its keys, expected queries and the invariants kept by code — `docs/schema.md` |
| Clients of other services, service token source | `github.com/fin-assist/go-grpc/client` |
| Zvonok V1 client and checked HTTP postback | `internal/adapter/zvonok/`, `internal/verification/check.go`, `docs/zvonok.md` |
| Runtime fake, synthetic namespace, production proof protection | `internal/adapter/fakephone/`, `internal/verification/test_phone.go`, `docs/fake-phone.md` |
| Public callback pool, transactional selection and rolling outcomes | `internal/verification/numbers.go`, `internal/adapter/postgres/verification_numbers.go` |
| Background jobs | `github.com/fin-assist/go-worker` |
| Configuration: every variable with its default and description | `internal/config/vars.go` (`Vars`); generated tables — `docs/config.md` |
| Request-scoped values (request ID, caller, user, bank-bff hints) | `github.com/fin-assist/go-grpc/reqctx` |
| Authentication limits, fixed windows and commit-aware metrics; integration rules | `internal/ratelimit/`, `internal/adapter/postgres/limits.go`, `docs/limits.md`; `apitest` pins the limiter clock (`app.WithLimitClock`, `Env.LimitNow`, FIN-313) so a scenario never straddles a window |
| Logger, metrics endpoint, readiness | `internal/logx/`, `internal/ops/` |
| Image, local run, stand | `Dockerfile`, `compose.yaml`, `Makefile`, `deploy/systemd/` |
| CI, lint | `.github/workflows/ci.yml`, `.golangci.yml`, `.squawk.toml` |
| Service versions, release notes and repeatable GitHub Release | `CHANGELOG.md`, `docs/releasing.md`, `.github/workflows/release.yml`, `scripts/changelog.py` |
| Dependency updates | `renovate.json` |
| PR review by Claude | `.github/workflows/claude.yml` |

### Stack

Versions are in `go.mod` (Go toolchain included), `.github/workflows/ci.yml` and `Makefile` (golangci-lint), `compose.yaml`
and `migratetest.DefaultImage` (PostgreSQL). Main pieces: grpc-go, protobuf, protovalidate, pgx v5, golang-migrate,
prometheus client_golang, testcontainers-go, squawk; the fin-assist libraries above. A new dependency is named in
the PR with why it is needed.

## Build in an agent container

- Go: the toolchain from `go.mod` is downloaded by `GOTOOLCHAIN=auto` (the default) on the first `go` command.
- `fin-assist/*` modules come from git, not the proxy: `GOPRIVATE=github.com/fin-assist/*` (set by `Makefile`; export
  it for plain `go` commands).
- Docker is usually absent in an agent container, so testcontainers cannot start PostgreSQL. Run a local server and
  point the tests at it: `TEST_DATABASE_URL='postgres://postgres@localhost:<port>/postgres?sslmode=disable' make test`.
  Without either, integration tests are skipped locally and fail in CI (`CI` set) — a skip is not a pass.
- `make lint` runs the pinned golangci-lint through `go run`; no separate install.
- `make cover` (or `go test -coverpkg=./... -coverprofile=cover.out ./...`) needs the `covdata` tool, which a
  toolchain downloaded by `GOTOOLCHAIN` lacks ("no such tool covdata"): build it once with
  `T=$(go env GOTOOLDIR); chmod u+w "$T"; go build -o "$T/covdata" cmd/covdata`.
- The image needs Docker; without it say in the PR that the image was built only by the CI job `image`.
- Every test or lint run gets a timeout (`timeout 600 make test` or the tool's own): a hung run blocks the session.
- A local `serve` for a smoke check: `ENV=dev AUTH_MODE=insecure-dev SERVICE_NAME=user DB_PRIMARY_DSN=… make run`;
  `/readyz` on `HTTP_ADDR` answers when the database does and drops to 503 at SIGTERM, before `SHUTDOWN_DELAY` (FIN-289).

## Principles

Priorities when they pull apart, in this order:

1. **Test coverage** — a change without its tests (Tests below) is not done, however good the code.
2. **Clarity** — code a reader understands without the author: plain control flow, explicit names, no cleverness.
3. **Security** — the Security rules of the review are requirements, not trade-offs; this place in the order decides
   only between options that all meet them.
4. **Performance** — optimise when a measurement shows the need, not in advance; a faster but less clear version
   comes with the benchmark that justifies it.

- **No frameworks.** No web or RPC frameworks (gin, echo, fiber, go-kit), no ORM (gorm, ent), no DI containers or
  generators (fx, wire, dig), no config frameworks (viper): the standard library, grpc-go and explicit wiring in
  `internal/app`. `depguard` in `.golangci.yml` rejects the common ones.
- **Standard library first.** Use `net/http`, `log/slog`, `encoding/json`, `errors`, `testing`, `context`,
  `database/sql` interfaces and the rest of the standard library wherever they do the job; tests use `testing` (no
  testify, gomock). A third-party package only where the standard library has no equivalent — the PostgreSQL driver
  (pgx), gRPC and protobuf, protovalidate, golang-migrate, the Prometheus client, testcontainers — and a new one is
  named in the PR with why the standard library is not enough.
- **SOLID, the Go way:**
  - single responsibility — a package and a type have one reason to change: a usecase does not format messages, a
    handler does not hold business rules, a repository does not decide;
  - open/closed — new behaviour is a new type behind an existing interface (a repository, a client, a job), not a
    flag or a type switch in old code;
  - Liskov — every implementation of an interface keeps its contract, fakes in tests included: the same errors for
    the same cases, no panics where the real one returns an error;
  - interface segregation — small interfaces declared by the consumer with only the methods it calls;
  - dependency inversion — usecases depend on those interfaces, never on adapters; `wire()` passes the
    implementations in.

## Writing code

- Code, comments, identifiers, commit messages — English. `docs/*.md`, `README.md`, PR title and description —
  Russian. Comments where the logic is not obvious; a doc comment on every exported identifier.
- Layers: `domain` ← `<feature>` ← adapters ← `cmd`. A usecase declares the interface it needs (consumer side) and
  never imports an adapter, pgx or gRPC; a handler converts messages and calls a usecase; `wire()` connects them.
- Errors to callers: only `finerr` constructors (`finerr.NotFound()`, `finerr.LimitExceeded(resetsAt)`).
  `status.Error`, `status.New`, `errdetails.ErrorInfo` are forbidden by the linter; an internal refusal is a bare
  code through `bare()` in go-grpc/server. A new client reaction needs a new code in `contracts` first. A
  handler may return a `finerr` error wrapped with `%w` or any other error: the interceptor sends the caller the
  unwrapped status or a bare `INTERNAL` (`CANCELLED` / `DEADLINE_EXCEEDED` for context errors) and records only safe event/code/reason, omitting raw errors and panic payloads (`docs/server.md`, step 10).
- Identity: the user of a user call is `reqctx.UserID(ctx)` (the verified `sub`), never a request field; a system
  call takes `user_id` from the request. Every new method gets its row in `AccessPolicy()`, copied from
  grpc-api.md 3.3; the server does not start without it.
- **SQL construction:** write fixed-structure queries as plain SQL with parameters (`$1`, `$2`, … for
  PostgreSQL). Use [Squirrel](https://github.com/Masterminds/squirrel) (`github.com/Masterminds/squirrel`) only
  when assembling a query dynamically, such as optional predicates or varying column lists. Changing argument
  values alone does not make a query dynamic. This limited use is an approved exception to "standard library
  first". Parameterize values; keep identifiers and structural fragments constant or from an explicit allowlist.
  Use `PlaceholderFormat(sq.Dollar)` for PostgreSQL builders. If a dynamic query cannot be expressed by the
  builder, explain the limitation beside its manual construction and preserve parameterization.
- Transactions (FIN-240): `postgres.DB` delegates to go-postgres; each pool owns its Manager.
  Auth idempotency, auth/Start repositories, guard, publisher, limiter store and immediate fake
  confirmation use the dedicated auth DB. Never reuse an ordinary-pool repository inside that
  transaction: foreign contexts fail closed. Callback reports and independent IP attempts retain
  the ordinary pool. SessionTransactions selects the verified endpoint's outer mapper; inner
  usecase Run joins without installing a mapper. Profile RunGrant alone maps consent constraints.
  Match SQLSTATE and constraint together; preserve unknown causes and never expose PgError.Detail.
- Database: SQL with parameters only (`$1`), table and column names are constants. A usecase opens the transaction
  with `db.Run`; repositories take `db.Writer(ctx)` / `db.Reader(ctx)`; no calls to other services inside `Run`.
  Migrations follow `docs/migrations.md` (two releases for an incompatible change, `CONCURRENTLY` alone in a file).
  The schema holds only primary keys, unique indexes, foreign keys with their cascades, NOT NULL and defaults: no
  CHECK, EXCLUDE, triggers or functions (owner's decision of 19.10.2026, FIN-112). Formats, state rules and
  notifications are code; `docs/schema.md`, «Инварианты в коде сервиса», lists them.
- Clients of other services: `client.Dial` with `Retry` listing only reads and methods idempotent by construction;
  the context carries the deadline.
- Time: "now" in business logic comes from an injected clock (`func() time.Time` or an interface passed by `wire()`),
  never `time.Now()` inside a usecase or repository query: tests pin the date, and month and year boundaries must be
  testable. Calendar days and months are computed in the user's profile time zone (grpc-api.md 8), never in
  `time.Local` or the server's zone; `now()` in SQL only for technical columns (`created_at`, outbox schedule).
  Durations for metrics and timeouts may use `time.Now()` / `time.Since`.
- Money: integer minor units (`int64`, `_minor`), never floats; the sign follows the contract.
- Tolerance of what the service does not know yet: an enum value of another service it does not know (newer
  producer) follows the rule of that field in grpc-api.md 8 — hide the field or fail the call with an alert — never
  a panic or a silent default that changes meaning; an unknown field is ignored (protobuf does it). A new enum
  value or error metadata key follows the rollout order of grpc-api.md 5.1 and 8.
- Dev-only hooks — `AUTH_MODE=insecure-dev`, dev verifiers and tokens, reflection, test helpers — stay unreachable
  outside `ENV=dev`: config rejects them, and code must not add another way around it.
- Logs: `slog` `*Context` methods; no tokens, DSNs, statement contents or assistant dialogue in logs or errors.
  The debug log of a request masks `token` fields and clears every `bytes` field; other personal data in a
  request is logged at `debug` as is. Metric labels: bounded values only — never `user_id`, an operation ID or
  free text.
- Document in moderation: a doc comment on exported identifiers of packages others use, comments where the logic is
  not obvious (accounting rules, idempotency, locks, transaction boundaries), not on every function.

## Tests

Every change comes with its automated tests in the same PR — not in a follow-up. Three levels, each mandatory:

- **Unit tests — all new code.** Every new or changed function or method with logic (domain rules, usecases,
  mappers, validation, config, helpers, interceptors) has unit tests in its package: no database, no network, fakes
  of the interfaces the code declares; the main path and every branch and error the code handles.
- **Integration tests — all new functionality.** Every new or changed behaviour is tested against its real
  dependencies: repositories, migrations, transactions and locks against PostgreSQL (`testdb.DSN`), not mocks of
  pgx — the SQL itself is what is tested; clients of other services against a fake gRPC server on bufconn
  (retries, deadlines, metadata); jobs and the outbox through a real run. Every migration passes up → down → up
  (`migrations/migrations_test.go`).
- **API tests — all new functionality.** Every new or changed RPC is tested through the service's API as a caller
  sees it: `apitest.Start(t)` runs the whole assembly (`internal/app`: interceptors, access table, handlers,
  PostgreSQL with migrations) and the test calls it with the generated client as `bank-bff` (`apitest.User`) or
  another service (`apitest.Caller`). Assert the status code, `ErrorInfo.reason` (`apitest.Reason`) and metadata, and
  the response fields against the contract — not the message text. Cover the main path, every error the contract
  lists for the method, a foreign caller, another user's object and a deleted user.
- **API tests in `bank-bff` — every REST endpoint.** `bank-bff` is covered by API tests too, at its own API — the
  REST of `openapi.yaml` the mobile client calls: a real HTTP request to the `bank-bff` handler (`httptest`), the
  services behind it played by fake gRPC servers on bufconn that answer with success and with each registry error.
  Every response is validated against `openapi.yaml` (kin-openapi), and the test asserts the HTTP status,
  `error.code`, `details[]`, headers (`Retry-After`, `Idempotency-Key` handling) and SSE events — the main path, every
  error `docs/api.md` lists for the endpoint (including the translation of each `reason` by `finerr.ToREST` and an
  unknown one into 500), a missing or bad token, a missing `Idempotency-Key`, a body that fails the schema, an
  unsupported client version. Every operation of `openapi.yaml` that `bank-bff` serves has at least one API test.

Also:

- A bug fix comes with a test that fails without the fix and reproduces the reported case, at the lowest level that
  shows it, plus an API test when the caller saw the bug.
- Security-relevant behaviour (foreign caller, another user's object, deleted user, consent gates, hostile input)
  has negative tests: what must not happen is asserted — the handler did not run, no row was written, nothing leaked.
- Tests assert behaviour; a test that only executes lines or asserts on a fake it set up itself is not coverage.
  Run with `-race`; no sleeps for synchronisation — wait on a channel or a condition with a deadline.
- The measurable gate is at least 80% of all production statements independently in unit, integration and API
  suites. The same `-coverpkg=./...` denominator includes generated production code and `.go` test helpers;
  no exclusions to reach the threshold. Name every test `TestUnit*`, `TestIntegration*` or `TestAPI*` by its
  actual boundary, and every fuzz target `FuzzUnit*`, `FuzzIntegration*` or `FuzzAPI*` (its seed corpus runs
  in that suite); `make test-names` rejects any other `Test`/`Fuzz` name and any `Example…`, which would run
  outside all three suites (FIN-290). Direct handlers on fakes are unit; full-assembly jobs without public RPC are integration.
  Coverage percentage never substitutes for the use-case review.

## Checks

Run `make test` and `make lint` before pushing. `make cover-unit`, `make cover-integration`, `make cover-api`
run independent race-enabled uncached suites with an eight-minute test timeout. `make cover` runs every suite,
then the supplementary union. All require `jq`; integration/API need PostgreSQL 18 (Docker or TEST_DATABASE_URL).

`make test` and CI also run the Python script tests and check CHANGELOG format. Service releases follow
`docs/releasing.md`: prepare a dated version section in a reviewed PR, wait for post-merge CI, then the owner
pushes the tag. Agents never create, move or delete version tags. Release publishes GitHub metadata only;
image publication and deployment remain separate. Existing releases are preserved after validating the tag.

### Coverage reports (FIN-238)

Use pinned `go-test-coverage/v2@v2.20.1`: total >=80 independently for unit.out, integration.out and api.out.
Owner accepts its one-decimal rounding (79.95% passes as 80.0%). The old changed-line metric and custom parser
are removed. Built-in comma-separated profile merging produces combined coverage, never a substitute for
one failed suite. A JSON Go-test log must contain an actual passed test; missing, empty and header-only
profiles fail before analysis. Skipped or failed Test events fail the execution guard; `make test-names`
rejects Test/Fuzz names outside the three selectors (`^(Test|Fuzz)<Suite>`) and any Example. CI=true makes missing PostgreSQL fatal. CI retains the required `go` job,
collects all independent outcomes, fails if any gate fails, and uploads logs, profiles and built-in reports.
Forks need no PR-write permission. README documents the boundaries and outstanding baseline gaps.

## Workflow

- Branches: `<kind>/<slug>` — `feature/…`, `fix/…`, `tooling/…`; a PR targets `main`. Commits: imperative English
  subject (`Add …`, `Fix …`, `Address review: …`).
- Dependency updates come from Renovate (`renovate.json`; README «Библиотеки и обновления»): its `renovate/…`
  branches are the exception to the branch names and need no YouTrack issue while they change nothing but
  dependencies. FIN-211 permits merge/retry/close of allowed bot-only updates, including v0 minor/major changes,
  after required CI; deployment and rollout instructions in CHANGELOG/contracts remain the owner's responsibility.
  Human fixes make the PR manual and require a YouTrack issue. After automatic closure, create a separate fix PR
  with an issue rather than pushing to the closed Renovate branch; follow README dependency policy.
- PR title and description in Russian: what changed and why, deviations from the contracts or docs, how it was
  checked. A PR for a single issue starts its title with the key (`FIN-42 Поиск операций по сумме`); a PR for several
  issues lists all of them in the description, each linked (`https://finassist.youtrack.cloud/issue/FIN-<n>`).
- A dependency on a contract change: the contract PR is merged and `gen/go` tagged first, then the service PR bumps
  the module. Order of rollout for new enum values and error metadata — grpc-api.md 5.1 and 8.
- After the PR is open and all planned changes are pushed, request a review with one PR comment in Russian addressed
  to both reviewers; it starts with `@codex @claude`:

  ```
  @codex @claude проведите ревью этого PR по Code Review Rules в AGENTS.md, включая разделы Security и Tests.
  Замечания — на русском, inline на изменённых строках. Не придирайтесь к формулировкам документации и комментариев:
  замечание по тексту только если он искажает поведение кода. Особое внимание: <что именно и почему>.
  ```

  - `@claude` — `.github/workflows/claude.yml` (runs from the default branch), Claude Opus 5.5
    (`claude-opus-5-5`, `--effort high`; FIN-204). Needs the Claude GitHub App and the `CLAUDE_CODE_OAUTH_TOKEN` secret.
  - `@codex` — the Codex GitHub integration under the owner's ChatGPT subscription. Its model is chosen by OpenAI
    and cannot be pinned from here; a pinned model would need the OpenAI API with per-token billing, which was
    declined (FIN-103).
  - Claude review is requested only once `.github/workflows/claude.yml` is on `main`: its comment-triggered
    workflow runs from the default branch. Codex review is independent and is requested whenever its
    GitHub integration is available.
  - Replies in threads and other comments never mention `@codex` or `@claude`: any mention with text starts them
    again.
- Waiting for the review: react to the PR's GitHub events when the session receives them; otherwise check 3 minutes
  after the request, then every 2 minutes. A reviewer has answered when it posted its review or summary for the
  latest commit; for Codex (`chatgpt-codex-connector[bot]`) «Didn't find any major issues» or a 👍 without a comment
  also counts, only for the latest request/current commit. If Codex has no result after 5 minutes, request it
  again on the current SHA; repeat every 5 minutes until a result, an explicit integration error or the owner's
  stop command. Running and the eyes reaction are not results; no 15-minute cutoff applies to Codex.
  Other reviewers silent for 15 minutes: report that in the PR and to the owner, then stop waiting for them.
  The cycle ends when the requested reviewers answered the latest request and no thread is open (or their
  explicit error/allowed timeout was reported). Follow the owner's choice of reviewers for the session.
- Address findings in follow-up commits on the same branch, reply in the thread and resolve it, then request a
  review again. Agents never merge a PR; the owner does.

## Code Review Rules

Read by `@claude` (through `CLAUDE.md`) and by the Codex integration, which looks for this heading. Two areas get a
dedicated pass on every PR: Security and Tests.

### General

- Write all review comments in Russian (summary and inline). Identifiers and quotes stay as they are.
- Report only verified problems with a concrete failure scenario: which caller, request or data, what goes wrong.
  Skip style nits, naming preferences and wording of comments and docs unless the text misstates the behaviour.
- Weigh findings by the priorities of «Principles»: missing tests first, then clarity, then security beyond the
  mandatory rules, then performance; a performance finding needs a measurement or an evident cost (an N+1, an
  unbounded load), not a guess.
- Post each finding as an inline comment on the changed line. The summary lists what was checked, with a
  «Безопасность» part and a «Тесты» part; «not affected» is a valid answer, with one line why.
- Check that the PR references its YouTrack issue and updates `README.md`, this file and `docs/` when it changes
  what they describe.

### Security

The service handles a user's bank operations, phone number, statement files and assistant dialogue (personal data
under 152-ФЗ). A finding names what leaks or breaks and how a client, another user or a compromised service gets to
it. Check:

- Object access: every query by an object ID is scoped by the user (`WHERE user_id = $n` with the user from
  `reqctx.UserID` or the system call's `user_id`); another user's object gives `NOT_FOUND`, not its data or a
  different error that confirms it exists (IDOR / BOLA).
- Identity: the user never comes from a request field in a user call; a new method has its row in `AccessPolicy()`
  matching grpc-api.md 3.3 (callers and model); hints `x-client-ip`, `x-client-version`, `x-read-primary` are used
  only as the server package sets them; `x-client-version` never decides access or limits.
- Errors: client-facing errors only through `finerr`; `status.message`, `error` details and logs contain no SQL,
  stack traces, tokens, DSNs or other users' data; an internal refusal is a bare code.
- Secrets: tokens and DSNs only in headers, files or `*_FILE` variables, never in logs, metric labels, URLs or error
  texts. Owner-approved FIN-139 exception: authenticated Zvonok V1 GET reads put public_key
  in query as documented; the pinned HTTPS client refuses redirects and never exposes URLs
  or remote error text. Creation keeps the key in the multipart body; the masking list in `docs/server.md` covers new secret metadata or fields.
- SQL: parameters only; nothing from a request selects a table, column, `ORDER BY` fragment or file path.
- Untrusted input: bounds come from protovalidate annotations in `contracts`; a handler that reads a size, count or
  page relies on them or checks itself; long-running or costly work (file parsing, LLM, calls, SMS) has a limit
  and a deadline.
- Transactions: an effect and its idempotency record or outbox row are written in one `db.Run`; no external call
  inside `Run`; a deleted user's data is not written back (tombstone).
- Clients: no user token forwarded to another service; retries only for reads and idempotent methods.
- Consent: a service that sends user data to the LLM checks the assistant consent first; an account is created only
  after the personal data consent; revoking a consent or deleting the account removes data as the contract says.
- Dev and test hooks (`insecure-dev`, dev verifiers, reflection, fixtures) cannot be enabled in `stage` or `prod`.
- Statement files and other uploads: size, element count and nesting depth are bounded; OFX 2.x (XML) is parsed with
  DTD and external entities disabled; a hostile file ends in a handled error, not a panic, a hang or memory growth.
- Trust boundaries: limits, quotas and permissions are enforced here, never left to the client or to `bank-bff`
  alone; `x-client-version` never decides access.
- Secrets in the repository: no real keys, tokens or DSNs with passwords (compose uses throwaway local ones).
- New dependencies: flag a new library that gets network, storage or data access, so the owner decides on it.

### Tests

The criterion is not a percentage of lines but whether the use cases of the changed functionality are covered; the
80% per-suite total-statements target in Tests is the author's minimum, and meeting it does not settle this check. For
each behaviour the PR adds or changes:

1. List its use cases from the code, the contract and the issue: the main path; alternative paths; every error a
   caller can reach (validation, not found, another user's object, consent missing, limit exceeded, a peer
   `UNAVAILABLE`, a database error, a cancelled context or an expired deadline); boundary values (zero, negative and
   very large amounts, dates at day, month and year boundaries, time zones, empty and maximal lists, long texts);
   repeated and concurrent calls (an idempotent retry with the same key, two writers at once); a deleted user.
2. Map each case to the test that covers it and check all three levels of Tests above: new code without a unit test,
   new functionality without an integration test or without an API test is a finding.
3. Post a finding for an uncovered case only when it is reachable in the changed code and its behaviour is added or
   changed by the PR; name the concrete case and the layer that should cover it. The list in step 1 is a prompt, not
   a checklist to report item by item; a case not worth a test is fine when the PR says why.

Also check that a test would fail if its case broke, that a bug fix's test reproduces the reported case, that every
migration passes up → down → up (an irreversible step marked as `docs/migrations.md` says), and that
security-relevant behaviour has negative tests.

### Consistency

- Field meanings, error codes, limits and call models match `fin-assist/contracts`; a deviation is stated in the PR.
- Names follow the existing ones; no second way to do what a package here already does (transactions, clients,
  config, logging).
- No framework, ORM or DI container; a new third-party dependency where the standard library would do is a finding.
- SOLID: business rules outside usecases, an interface declared by its implementation instead of its consumer, a
  usecase importing an adapter, or a fake that behaves unlike the real implementation is a finding.
- Metric labels stay bounded; a new metric has a help text and is listed in `docs/server.md`.

### Checklist

- New code has unit tests; new functionality has integration tests and API tests (`apitest`), see Tests.
- In `bank-bff`, a new or changed REST endpoint has API tests over HTTP with responses validated against
  `openapi.yaml`.
- A new gRPC method has its row in `AccessPolicy()` matching grpc-api.md 3.3.
- A new client-facing error goes through `finerr`; a new reason or metadata key exists in `contracts` first.
- A new query by object ID is scoped by the user and has a test with another user's ID.
- A new effect with an idempotency record or an outbox row is written in one `db.Run`, and a retry test shows it is
  not applied twice.
- A new migration follows `docs/migrations.md` and passes squawk and up → down → up.
- A new metric has bounded labels and is listed in `docs/server.md`; a new setting is declared in `Vars` and the
  tables of `docs/config.md` are regenerated, not edited by hand.
- Each suite independently meets the 80% total-statements target; lower coverage is explicitly reported as incomplete.

## Renovate automation

FIN-211: dependency-only Renovate PRs may be merged/closed by the reviewed `renovate-policy.yml` controller, after exact-head required CI and bot-only commit checks. Ordinary PRs and release-to-main PRs still require the owner. Do not enable native automerge or bypass protections. Preserve base policy in `renovate-base.json` (schedules, groups, allowed versions and manual Go upgrades); generated target/version approvals live on the separate `renovate-policy` branch. Failed exact values are isolated for explicit Dashboard approval; new values keep their original groups. AndroidX/Compose and mixed groups never auto-merge. See `docs/renovate-policy.md` for credentials, safeguards, bootstrap and verification.

FIN-216 public-number metrics are the explicit exception to fixed-only label values:
only provider-owned callback numbers, capped at 256 per process, then `other` plus an
observable overflow gauge. Caller phone numbers and call IDs remain forbidden labels.
Rolling 24h gauges come from primary, do not sum replicas. Exact public numbers in the
bounded outcome report are aggregate logs, not a per-request event audit. See docs/zvonok.md.

## Stage artifacts (FIN-148)

`scripts/stage_artifact.py` extracts the binary from the smoke-tested image; `docs/stage-artifacts.md` describes provenance, checks and retention. Keep its Python tests and CI image job in sync. Artifacts do not authorize deployment; only a successful main push CI is eligible for stage. GHCR/production delivery is still pending.
