---
name: database-performance-triage
description: Triages slow requests that might be the database, from the application side and across stacks - separates query time from connection-pool waits, lock waits, round-trip counts and app-side processing using traces, ORM query logs, pg_stat_statements, pg_stat_activity, MySQL performance_schema or the MongoDB profiler - then routes each finding to the right fix (index, query rewrite, N+1 batching, pool sizing, lock contention, caching). Use when an endpoint is slow and someone suspects the DB, latency spikes under load, the pool times out, or a team needs to know which query to fix first.
license: MIT
---

# Database performance triage

Done well means: you can say, with numbers, how much of the slow request is spent
waiting for a connection, executing queries, waiting on locks, and in the application,
and which one or two statements (by normalised fingerprint) account for most of the DB
time. Only then does anyone add an index or resize a pool.

## Split the latency

For one slow request, the DB-related time is:

`pool wait + (round trips x network RTT) + execution + lock wait + result transfer + ORM hydration`

| Observation | Points to |
|---|---|
| Hundreds of similar queries per request | N+1 / chatty access: `$n-plus-one-queries` |
| Few queries, one with high execution time | Query plan / index: `$postgres-query-tuning`, `$postgres-indexing`, `$mysql-tuning` |
| Queries fast in DB stats, slow in app traces | Pool wait, network, or ORM hydration of huge result sets |
| Latency spikes under load only, pool timeout errors | Pool exhaustion or long transactions holding connections: `$connection-pooling` |
| Same statement sometimes 5 ms, sometimes 5 s | Lock waits, plan flips, cache misses, or vacuum/IO pressure |
| DB CPU or IO saturated overall | Top queries by total time, then capacity |

## Workflow

```
- [ ] 1. Pick the slow endpoint(s) and their p50/p95/p99 from APM or logs
- [ ] 2. Get a trace or per-request query log: count, total DB time, slowest statement
- [ ] 3. Check pool metrics: active, idle, waiting, wait time, timeouts
- [ ] 4. Check DB-side stats: top statements by total and mean time, current waits and locks
- [ ] 5. Classify (table above) and hand off to the specific fix
- [ ] 6. After the fix: same endpoint, same load, compare DB time per request and p95
```

## Application side

- Tracing (OpenTelemetry, APM) shows DB spans per request; sort by span count and total
  duration. `$distributed-tracing` covers reading traces.
- ORM query logging in a dev or staging run of the endpoint:

| Stack | Turn on query logging |
|---|---|
| Django | `django.db.backends` logger at DEBUG, or django-debug-toolbar / `assertNumQueries` in tests |
| Rails | Development log shows SQL; `ActiveRecord::Base.logger`; Bullet gem for N+1 |
| SQLAlchemy | `create_engine(..., echo=True)` or the `sqlalchemy.engine` logger |
| Prisma | `new PrismaClient({ log: ['query'] })` |
| Hibernate / Spring | `spring.jpa.show-sql=true`, `hibernate.generate_statistics=true` |
| EF Core | `optionsBuilder.LogTo(Console.WriteLine)` |
| Go database/sql | Wrap the driver (e.g. OpenTelemetry instrumentation) or log in the repository layer |

Count queries per request and total DB time. Turn verbose logging back off afterwards;
never enable full SQL logging in production without approval (it logs data and costs
throughput).

- Pool metrics: HikariCP (`hikaricp_connections_pending`, `connectionTimeout`), node-postgres
  (`pool.waitingCount`), SQLAlchemy (`pool.status()`), PgBouncer `SHOW POOLS;` (`cl_waiting`).
  Any sustained waiting count means requests queue for connections.

## Database side (read-only queries)

PostgreSQL (needs the `pg_stat_statements` extension enabled):

```sql
-- Top statements by total time (what costs the DB most)
SELECT left(query, 120) AS q, calls, round(total_exec_time) AS total_ms,
       round(mean_exec_time::numeric, 1) AS mean_ms, rows
FROM pg_stat_statements ORDER BY total_exec_time DESC LIMIT 15;

-- What is running or waiting right now
SELECT pid, state, wait_event_type, wait_event, now() - query_start AS running_for, left(query, 100)
FROM pg_stat_activity WHERE state <> 'idle' ORDER BY running_for DESC;

-- Who blocks whom
SELECT pid, pg_blocking_pids(pid) AS blocked_by, left(query, 100)
FROM pg_stat_activity WHERE cardinality(pg_blocking_pids(pid)) > 0;

-- Long idle-in-transaction sessions holding locks and connections
SELECT pid, now() - xact_start AS xact_age, left(query, 100)
FROM pg_stat_activity WHERE state = 'idle in transaction' ORDER BY xact_age DESC;
```

MySQL 8:

```sql
SELECT DIGEST_TEXT, COUNT_STAR, ROUND(SUM_TIMER_WAIT/1e12, 1) AS total_s,
       ROUND(AVG_TIMER_WAIT/1e9, 1) AS avg_ms, SUM_ROWS_EXAMINED
FROM performance_schema.events_statements_summary_by_digest
ORDER BY SUM_TIMER_WAIT DESC LIMIT 15;

SELECT * FROM sys.innodb_lock_waits\G
```

MongoDB: `db.setProfilingLevel(1, { slowms: 100 })` then `db.system.profile.find().sort({ millis: -1 }).limit(10)`;
check `planSummary` for `COLLSCAN`. Changing the profiling level is a config change;
ask before doing it in production.

Running `EXPLAIN ANALYZE` executes the statement. On writes (`UPDATE`, `DELETE`,
`INSERT`) run it only inside a transaction you roll back, and only with approval on
production; read-only replicas or staging copies are better.

Deeper Postgres health (bloat, vacuum, cache hit ratio) lives in `$database-observability`.

## Prioritise

Rank by **total time** (calls x mean), not by the single slowest query: a 3 ms query run
2 million times a day outweighs a 4 s report run twice. Then weigh user impact: a query on
the checkout path beats a nightly batch.

## Quick fixes by class

| Class | First move |
|---|---|
| N+1 | Eager load / batch (`select_related`, `includes`, `IN (...)`, DataLoader) |
| Missing index | Index matching the WHERE + ORDER BY, created concurrently/online |
| Over-fetching | Select needed columns, paginate, avoid hydrating thousands of ORM objects |
| Pool exhaustion | Shorten transactions, move external calls out of transactions, then size the pool to what the DB can serve |
| Lock contention | Smaller transactions, consistent lock order, `SKIP LOCKED` for queues, avoid hot-row counters |
| Repeated identical reads | Cache with explicit invalidation (`$redis-caching`) |
| Analytics on OLTP | Move to a replica or warehouse |

Schema changes, index builds on large tables and pool resizing affect production; show
the plan and get confirmation before applying them.

## Verification

- Same endpoint under the same load: queries per request, DB time per request, and p95
  before vs after.
- `pg_stat_statements` (reset or compare deltas over equal windows) shows the target
  statement's total time dropped.
- Pool waiting count stays at zero at peak.

## Report template

```markdown
## DB triage: GET /api/orders (p95 1.9 s at 300 rps)
Per request (trace median): 214 queries, DB 1,320 ms, pool wait 240 ms, app 180 ms
Top statements by total time (pg_stat_statements, 24 h):
1. SELECT ... FROM order_items WHERE order_id = $1 - 41% of DB time, 9.8M calls (N+1)
2. SELECT ... FROM orders WHERE customer_id = $1 ORDER BY created_at DESC - Seq Scan, 22%
Pool: max 20, waiting peaks at 35 during traffic peaks
Plan: batch order_items (n-plus-one-queries); index (customer_id, created_at) concurrently; re-check pool after
Result: 6 queries/request, DB 140 ms, p95 1.9 s -> 280 ms
```
