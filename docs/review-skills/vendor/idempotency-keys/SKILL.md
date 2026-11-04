---
name: idempotency-keys
description: Makes unsafe API operations safe to retry using Idempotency-Key headers, a key store in the same database as the business write, request fingerprinting, in-progress locking, stored-response replay and expiry, plus natural idempotency via unique constraints and client-generated IDs. Use when building payment, order, transfer or provisioning endpoints, when clients or gateways retry POSTs, when duplicate charges or double-created records appear, or when a downstream provider needs idempotency keys passed through.
license: MIT
---

# Idempotency keys

An operation is idempotent when a client can send the same request any number of times,
concurrently or after a timeout, and the system performs the side effect once and
returns the same answer each time. Duplicates are prevented by the database, not by
hoping retries do not overlap.

## Choose the mechanism

| Situation | Mechanism |
|---|---|
| Resource identity can come from the client | Client-generated ID + `PUT /orders/{id}` or a unique constraint; no key table needed |
| Business has a natural unique key (invoice number, external event ID) | Unique constraint on it; on conflict return the existing record |
| POST that creates or charges, client cannot name the resource | `Idempotency-Key` header with a stored-response table |
| Multi-step operation calling external providers | Idempotency key plus recovery points per step, and pass a derived key to each provider |
| Message or webhook consumer | Dedupe table keyed by message/event ID (see webhook-receivers, background-jobs) |

Prefer the simplest row that fits; the header-and-table approach is for when nothing
natural exists.

## Workflow

```
- [ ] 1. Find the unsafe operations and current retry sources
- [ ] 2. Define the key contract
- [ ] 3. Create the key store
- [ ] 4. Implement the claim / execute / record flow
- [ ] 5. Propagate keys to downstream providers
- [ ] 6. Test concurrency, replay and mismatch
```

### 1. Find unsafe operations

Read `AGENTS.md`, then list POST/PATCH handlers that create money movements, orders,
emails, external resources or quota consumption. Note who retries them: mobile clients
on flaky networks, SDK retry policies, API gateways, queue redelivery, users
double-clicking.

### 2. Key contract

- Header: `Idempotency-Key`, client-generated, 1-255 characters; recommend UUIDv4/v7.
- Scope keys by caller (account or API key) so two customers cannot collide.
- The client generates one key per logical operation and reuses it on every retry of
  that operation. A new key per HTTP attempt defeats the mechanism.
- Retention: 24 hours minimum; match the longest client retry horizon.
- Responses, aligned with the IETF idempotency-key draft and common practice:

| Case | Response |
|---|---|
| First request | Execute, store, return the result |
| Same key, same payload, completed | Replay stored status and body; add `Idempotent-Replayed: true` |
| Same key, still in progress | 409 Conflict with `Retry-After: 1` |
| Same key, different payload | 422 Unprocessable Content (key reuse error) |
| Key missing on an endpoint that requires it | 400 with a clear error |

### 3. Key store

Store keys in the same database and transaction as the business write whenever possible,
so "record created" and "key completed" commit together.

```sql
CREATE TABLE idempotency_keys (
  account_id     bigint       NOT NULL,
  idem_key       text         NOT NULL,
  request_hash   bytea        NOT NULL,
  method         text         NOT NULL,
  path           text         NOT NULL,
  state          text         NOT NULL DEFAULT 'started',  -- started | completed
  recovery_point text         NOT NULL DEFAULT 'started',
  locked_until   timestamptz,
  response_code  int,
  response_body  jsonb,
  created_at     timestamptz  NOT NULL DEFAULT now(),
  PRIMARY KEY (account_id, idem_key)
);
CREATE INDEX idempotency_keys_created_at ON idempotency_keys (created_at);
```

Use Redis (`SET key value NX PX ttl`) only when the protected side effect also lives
outside the relational database; otherwise you lose atomicity.

### 4. Claim, execute, record

```ts
const hash = sha256(canonicalJson({ method: req.method, path: req.path, body: req.body }));

await db.tx(async (tx) => {
  const claimed = await tx.query(
    `INSERT INTO idempotency_keys (account_id, idem_key, request_hash, method, path, locked_until)
     VALUES ($1, $2, $3, $4, $5, now() + interval '60 seconds')
     ON CONFLICT (account_id, idem_key) DO NOTHING
     RETURNING idem_key`,
    [accountId, key, hash, req.method, req.path],
  );

  if (claimed.rowCount === 0) {
    const { rows: [row] } = await tx.query(
      `SELECT * FROM idempotency_keys WHERE account_id = $1 AND idem_key = $2 FOR UPDATE`,
      [accountId, key],
    );
    if (!row.request_hash.equals(hash)) return reply(422, problem('idempotency_key_reused'));
    if (row.state === 'completed') return replay(row.response_code, row.response_body);
    if (row.locked_until > new Date()) return reply(409, problem('request_in_progress'), { 'Retry-After': '1' });
    // lease expired: previous attempt crashed; take over and resume from row.recovery_point
  }

  const result = await createOrder(tx, req.body);            // business write, same tx
  await tx.query(
    `UPDATE idempotency_keys
       SET state = 'completed', response_code = $3, response_body = $4, locked_until = NULL
     WHERE account_id = $1 AND idem_key = $2`,
    [accountId, key, 201, result],
  );
  return reply(201, result);
});
```

Rules:

- In this single-transaction form, a concurrent duplicate blocks on the unique index
  until the first transaction commits, then replays. Set a `lock_timeout` so it fails
  fast instead of queueing. The 409 and lease-takeover branches matter in the
  multi-phase form below, where the claim commits in its own transaction first.
- Fingerprint the canonicalized request (sorted JSON keys, method, path), not raw bytes;
  harmless whitespace differences should not trigger 422.
- Store the final response for completed business outcomes, including deterministic 4xx
  results (for example "card declined"), so retries see the same answer.
- On transient failures before any side effect committed (database timeout, 503 from a
  dependency), roll back so the key is not stored and the client can retry the same key.
- With external calls in the middle, a single transaction cannot cover everything:
  split into phases, update `recovery_point` after each committed phase, and on
  takeover resume from the recorded point instead of starting over.

### 5. Downstream providers

- Pass a derived key to providers that support it (Stripe and many payment APIs accept
  `Idempotency-Key`): for example `${idem_key}:charge`. A retry then cannot double charge
  even if your record of the first call was lost.
- Emails and messages sent as side effects go through an outbox written in the same
  transaction (see outbox-pattern), so replays do not resend them.

### 6. Expiry

Delete expired keys with a scheduled job in batches:

```sql
DELETE FROM idempotency_keys
WHERE ctid IN (
  SELECT ctid FROM idempotency_keys
  WHERE created_at < now() - interval '24 hours'
  LIMIT 5000
);
```

(Postgres-specific `ctid` batching; on other databases batch by primary key.)

## Gotchas

- Checking for an existing key and inserting it in two statements without a unique
  constraint lets two concurrent requests both proceed.
- Keys scoped globally rather than per account let one customer's key block another's.
- Middleware that caches responses in memory per instance does nothing for retries that
  land on a different instance.
- Replaying stored responses must also replay the status code and relevant headers
  (`Location`), not just the body.
- Retrying with the same key but a different amount is a client bug; answering 422
  surfaces it instead of silently returning the old result.
- Idempotency does not make the operation safe to run twice with different keys:
  double-clicks in the UI need the client to reuse one key per form submission.

## Verification

- Concurrency test: send the same keyed request 20 times in parallel; exactly one
  business record exists, one response is the original, others are replays or 409s.
- Replay test: repeat after completion; identical status, body and `Location`, with
  `Idempotent-Replayed: true`.
- Mismatch test: same key, different body returns 422.
- Crash test: kill the handler after the first phase (or simulate with a thrown error)
  and confirm a retry after the lease completes the operation exactly once.
- Check the expiry job deletes old keys and that the table has an index supporting it.

## Output template

```markdown
## Idempotency for <endpoint>
- Mechanism: <client ID | unique constraint | Idempotency-Key table>
- Key scope and retention: <account, 24h>
- Fingerprint: <fields hashed>
- Responses: replay / 409 in-progress / 422 mismatch / 400 missing
- Downstream keys: <provider: derived key format>
- Tests: concurrency, replay, mismatch, crash-resume
```
