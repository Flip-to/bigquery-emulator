# Flip-to fork of bigquery-emulator

`flipto/main` is upstream `goccy/bigquery-emulator` plus fixes we need before
upstream merges them. `main` tracks upstream unchanged.

The SQL engine comes from our googlesqlite fork through a `replace` directive in
`go.mod`:

```
replace github.com/goccy/googlesqlite => github.com/Flip-to/googlesqlite <pseudo-version of its flipto/main>
```

## Image

Every push to `flipto/main` publishes `ghcr.io/flip-to/bigquery-emulator:flipto`
(moving) and `:flipto-<sha7>` (pinned). Pin the sha tag in anything that must be
reproducible.

The package is **private** (the org does not allow public packages), so pulling
needs a login.

**Developers**, once per machine: create a classic personal access token with only
the `read:packages` scope (GitHub → Settings → Developer settings → Personal
access tokens → Tokens (classic)), authorize it for the Flip-to org (SSO), then:

```
echo <token> | docker login ghcr.io -u <github-username> --password-stdin
docker run -p 9050:9050 ghcr.io/flip-to/bigquery-emulator:flipto --project=<project> --host=0.0.0.0 --port=9050
```

**GitHub Actions in another Flip-to repo** (e.g. `flipto-dbt`): no personal token.
An org admin grants the repo read access once, on the package page
(https://github.com/orgs/Flip-to/packages/container/package/bigquery-emulator →
Package settings → Manage Actions access → add the repo with role **Read**).
The workflow then logs in with its own `GITHUB_TOKEN`:

```yaml
permissions:
  contents: read
  packages: read
steps:
  - uses: docker/login-action@v3
    with:
      registry: ghcr.io
      username: ${{ github.actor }}
      password: ${{ secrets.GITHUB_TOKEN }}
  - run: docker run -d -p 9050:9050 ghcr.io/flip-to/bigquery-emulator:flipto-<sha7> --project=<project> --host=0.0.0.0 --port=9050
```

No login at all: build from the public repo instead
(`docker build -t bigquery-emulator:flipto https://github.com/Flip-to/bigquery-emulator.git#flipto/main`).

Windows native binary: set `TZDIR` to a drive-relative path holding zoneinfo
(for example Go's `lib/time/zoneinfo.zip` unzipped to `C:\tmp\zoneinfo`, then
`TZDIR=/tmp/zoneinfo`); without it the analyzer fails to initialize.

## Where does the time go (request timing logs)

The default log level is `error`, so nothing is logged per request. Start with
`--log-level=info` (add `--log-format=json` for one JSON object per line) to get
one access line per request, written after the handler returns:

```
{"L":"INFO","M":"POST /bigquery/v2/projects/test/queries","method":"POST","path":"/bigquery/v2/projects/test/queries","query":"","status":200,"bytes":207,"duration_ms":20.88,"lock_wait_ms":0,"handler_ms":20.88}
```

- `duration_ms`: total time from arrival to the end of the handler;
- `lock_wait_ms`: time spent waiting for the global sequential lock (queueing
  behind other requests); `handler_ms` is the rest, the time holding the lock;
- `status`, `bytes`: HTTP status and response body size.

With `--log-level=debug`, jobs.query and jobs.insert (query jobs) add one more
line per job with the phase timings (`prepare_ms`, `execute_ms` = analyze +
run, `result_write_ms` (destination table, jobs.insert only), `job_write_ms`,
`commit_ms`, `catalog_sync_ms`, `format_ms`), the job ID and the first 80
characters of the SQL.

## Carried changes

| change | repo | upstream |
|---|---|---|
| googlesqlite bumped to Flip-to/googlesqlite a2b2882: `googlesqlite_catalog` has an index on `updatedAt`, so the catalog sync that runs before every analyzed statement no longer scans every stored table spec; with 60 tables of 250 columns, `SELECT 1` through the emulator went from 176 to 85 ms and a CTAS from 360 to 190 ms (Flip-to/googlesqlite#39) | driver | none |
| the default temp-file storage keeps WAL mode, and with it concurrent reads: its fsync-skipping DSN no longer sets `journal_mode(memory)`, which made `withConcurrencyPragmas` skip WAL so every request ran exclusively. The `FindProject` row cache is bypassed while a transaction that wrote metadata is open, so rows read before a commit are never served after it (the commit precedes the end-of-transaction hook; under concurrent requests a dataset create racing `jobs.insert` could read a stale project). Tests `TestTempStorageSkipsFsync` (asserts WAL), `TestRowCacheInvalidation` | emulator | none |
| requests run concurrently: the global request mutex is now a read/write lock. GET requests (tables.get, jobs.get, getQueryResults, the list calls) and query jobs (jobs.query, jobs.insert) whose text is one SELECT with no destination table share it; DDL, DML, loads, inserts, patches, deletes and anything else take it alone. A shared query job records itself afterwards under a separate job-write mutex; on-disk databases now open in SQLite WAL mode with a busy timeout so that write runs beside the readers (in-memory databases keep every request exclusive: the SQLite driver has no shared cache, so each pooled connection to `file::memory:` is a separate empty database). Background query jobs from jobs.insert take the same lock mode themselves. Query-job bodies are read before any lock is taken. jobs.query SELECT throughput, 1 to 8 clients: 5.4 to 27.8 queries/s (`BenchmarkConcurrentSelect`; the old mutex stayed at about 6). Test `TestConcurrentClients` (8 clients mixing SELECT, jobs.insert + getQueryResults, tables.get/list, CREATE/INSERT/DROP; same results as serial, clean under `-race`) | bigquery-emulator | none (fork only) |
| `FindProject`, which runs on every request, reads the project, dataset, table, model and routine rows through an in-memory row cache instead of five analyzed SQL queries. Only raw column values are cached; objects are decoded fresh each time, so handler mutations never reach the cache. Every metadata write invalidates it, and so does the end (commit or rollback) of a transaction that wrote metadata. With 60 tables of 250 columns: GET dataset 120 → 17 ms, GET table 127 → 37 ms, SELECT 1 152 → 54 ms (with Flip-to/googlesqlite#39 as well: 18 / 29 / 37 ms). Test `TestRowCacheInvalidation` | emulator | none |
| the default temp-file storage (CLI without `--database`, `server.TempStorage`) opens SQLite with `synchronous(off)` and `journal_mode(memory)`: the file is deleted at shutdown, so fsyncing it on every commit bought nothing. Median per request on Windows: SELECT 17.5 → 10.5 ms, INSERT 16.1 → 9.5 ms, CTAS 32.5 → 19.1 ms (in-memory storage: 9.5 / 9.3 / 17.3 ms). Regression test `TestTempStorageSkipsFsync` | emulator | none |
| getQueryResults on a finished job answers at once: `Job.Wait` slept a 100 ms poll tick before its first look (about 450 ms of the 1.85 s a dbt unit test spends in the emulator), and for a missing job it spun until the client's deadline; jobs run synchronously, so it is now a single lookup. Regression test `TestGetQueryResultsOfFinishedJobDoesNotWait` | emulator | none |
| `ALTER TABLE ... ADD COLUMN [IF NOT EXISTS]` adds the column (storage and table metadata; was a silent no-op); `CREATE SCHEMA [IF NOT EXISTS] ... OPTIONS (location=...)` creates the dataset with its location (was a silent no-op); routines created with `CREATE FUNCTION` / `CREATE TABLE FUNCTION` or `routines.insert` are returned by `routines.get` / `routines.list` (was 404 / empty) and removed again by `DROP`; new `INFORMATION_SCHEMA.ROUTINES`, `INFORMATION_SCHEMA.PARTITIONS` and `<dataset>.__TABLES__`; `INFORMATION_SCHEMA.TABLES.creation_time` is a TIMESTAMP, not a STRING. Regression tests in `server/emulator_gaps_test.go`. Needs the googlesqlite change that reports these statements through `ChangedCatalog` (Flip-to/googlesqlite `claude/emulator-fixes`) | emulator, driver | none |
| memory stays bounded over long runs (flipto-dbt harness: 27 MiB fresh, 895 MiB after 6,136 jobs.query requests): the growth was the driver's wasm heap (every parsed statement's AST stayed in a long-lived arena, fixed in googlesqlite); on the emulator side the recent-jobs cache is now also bounded by the encoded size of the cached jobs (64 MiB), not only by count, so large results of old jobs are evicted from memory and read back from the jobs table on demand. Regression tests `TestQueryMemoryStaysBounded` (3,500 varied jobs.query requests; jobs.get and getQueryResults of the last job) and `TestCurrentTimeStableThroughJobsQuery` (one current time per statement, the harness probe current_ts_stable) need the googlesqlite bump that carries the driver fix | emulator, driver | none |
| a job whose transaction rolls back (or fails to commit) no longer stays in the recent-jobs cache: jobs enter the cache only on commit, so a client retry with the same job ID is not rejected as "job ... is already created"; server-generated job IDs use the runtime-seeded math/rand/v2 source instead of a time^pid seed identical to the Go client's, which made the first jobs.query ID collide with a client jobs.insert ID in-process (flaky TestAnonymousColumnNames) | emulator | none |
| per-request cost no longer grows with the job history: the project is loaded without its jobs, and recent jobs are served from a bounded cache instead of a full scan of the jobs table (flipto-dbt L12: SELECT 1 went from 0.25 s to 6 s after ~1,750 queries) | emulator | none |
| per-request access log (method, path, status, bytes, duration, wait for the global lock) at `--log-level=info`, and per-phase timings of query jobs at `--log-level=debug`; was a method+path line before the handler ran. Test `TestRequestTimingLogs` | emulator | none |
| googlesqlite bumped to Flip-to/googlesqlite 249671c: single-reference CTEs too costly to inline are materialized, so wide CASE chains no longer exhaust the 256 MiB wasm SQLite heap (`sqlite3: out of memory` on the flipto-dbt snowplow sessions / page_views models) (Flip-to/googlesqlite#31) | driver | goccy/googlesqlite#111 |
| googlesqlite bumped to Flip-to/googlesqlite e3fc9a7: MERGE accepts a subquery in USING (dbt-bigquery incrementals and the snowplow_unified manifest update failed with `MERGE: source must be a single-table reference`) (Flip-to/googlesqlite#29) | driver | goccy/googlesqlite#110 |
| googlesqlite bumped to Flip-to/googlesqlite 185ee0f: long compound SELECT chains (210+ UNION ALL arms) are nested so wasm SQLite no longer overflows its stack; 1000 arms per side run in 0.45 s, was 600+ s and an error (Flip-to/googlesqlite#26) | driver | goccy/googlesqlite#109 |
| googlesqlite bumped to Flip-to/googlesqlite 2c04e36: INTERVAL, RANGE, GEOGRAPHY and JSON-text values render exactly as BigQuery (TO_JSON_STRING, CAST AS STRING, FORMAT, WKT, GeoJSON) | driver | none |
| googlesqlite bumped to Flip-to/googlesqlite ac67606: reference-docs examples as regression tests and the driver fixes they found (array subscripts, WITH expressions, JSON / LAX functions, CAST to INTERVAL, numeric CAST FORMAT, geography) | driver | none |
| googlesqlite bumped to Flip-to/googlesqlite e126da1: compliance batch 3, literals folded again with UTC re-evaluation, 19 flipto-dbt divergences fixed, golden probe regression test (6,128/6,136) | driver | none |
| googlesqlite bumped to Flip-to/googlesqlite 5dce62e: the native binary starts on Windows without TZDIR (embedded zoneinfo) | driver | none |
| googlesqlite bumped to Flip-to/googlesqlite 86c0546 (compliance batch 2: MATCH_RECOGNIZE, pipe/DML, KEYS/AEAD, KLL, scalars, BOOL in containers) | driver | none |
| googlesqlite bumped to Flip-to/googlesqlite aa4d0f4 (compliance batch); a NULL element in a result array is an error, as in BigQuery ("Array cannot have a null element; error in writing field ...") | driver, emulator | none |
| useInt64Timestamp applies to TIMESTAMP values nested in STRUCT and ARRAY (Python client failed with "invalid literal for int()", flipto-dbt L4) | emulator | none |
| googlesqlite bumped to Flip-to/googlesqlite 589e255: bounded memory on DDL-heavy sessions (flipto-dbt L12) | driver | none |
| resumable upload Location built from the request Host (scheme + host:port the client used), not the listen address: with `--host=0.0.0.0` clients on Windows failed to PUT to `http://0.0.0.0:9050` (WinError 10049, flipto-dbt seed) | emulator | goccy/bigquery-emulator#528 |
| statement type and CTAS destination on query jobs | emulator | goccy/bigquery-emulator#518 (fixes #370) |
| CREATE OR REPLACE replaces an existing table | emulator | goccy/bigquery-emulator#519 |
| anonymous result columns named f0_, f1_, ... | emulator | goccy/bigquery-emulator#520 |
| nested wire format: anonymous STRUCT fields named _field_N, nested TIMESTAMP as epoch seconds, NULL arrays as [] | emulator | fork only |
| load jobs (dbt seed): schema type names and modes accepted in any case (dbt sends `string`/`int64`/`bool`, previously TYPE_UNKNOWN); omitted sourceFormat defaults to CSV; CSV honors fieldDelimiter (incl. `	`/`tab`), quote (any character or empty), nullMarker, encoding (UTF-8 BOM stripped, ISO-8859-1); rejected load data is reported as a DONE job with status.errorResult (kept by jobs.get) instead of an HTTP 500 that clients retry | emulator | none |
| failed query reported as invalidQuery, not the retried jobInternalError | emulator | goccy/bigquery-emulator#521; prior art goccy/bigquery-emulator#437 (closed unmerged) |
| temp tables kept out of the dataset metadata sync | emulator | goccy/bigquery-emulator#522 (needs goccy/googlesqlite#92) |
| LEFT JOIN UNNEST keeps empty/NULL-array rows | googlesqlite | goccy/googlesqlite#85 |
| SPLIT(NULL) is a NULL array | googlesqlite | goccy/googlesqlite#86 |
| TVF handle kept alive (GC use-after-free) | googlesqlite | goccy/googlesqlite#87 |
| ANY TABLE / TABLE<...> TVF parameters | googlesqlite | goccy/googlesqlite#88 (stacked on #87) |
| quoted dotted prefix in table/TVF paths | googlesqlite | goccy/googlesqlite#89 (stacked on #87) |
| DROP TABLE FUNCTION | googlesqlite | goccy/googlesqlite#90 |
| GROUP BY ALL | googlesqlite | overlaps goccy/googlesqlite#51 |
| MOD returns INT64 | googlesqlite | overlaps goccy/googlesqlite#60 |
| ARRAY_TO_STRING NULL | googlesqlite | overlaps goccy/googlesqlite#56 |
| comments in the DECLARE/SET pre-pass | googlesqlite | overlaps goccy/googlesqlite#55 |
| sub-catalogs without builtins (memory growth per DROP) | googlesqlite | overlaps goccy/googlesqlite#80 |
| dependency security bumps (grpc 1.83.2, otel/sdk, go-archive, tools) | both | fork only |
| LIKE `_` and backslash escapes | googlesqlite | overlaps goccy/googlesqlite#67 |
| ASSERT evaluates its condition (was a no-op: every ASSERT passed) | googlesqlite | goccy/googlesqlite#91 |
| end-of-script cleanup skips a temp table the script already dropped | googlesqlite | goccy/googlesqlite#92 |
| integral FLOAT64 in ARRAY/STRUCT stays FLOAT64 (was INT64, so division truncated) | googlesqlite | overlaps goccy/googlesqlite#66 (stacked on #63; both cherry-picked) |
| TO_JSON / TO_JSON_STRING quote DATE, DATETIME, TIME, TIMESTAMP | googlesqlite | goccy/googlesqlite#93 (reopens goccy/bigquery-emulator#428) |
| quoted dotted prefix in scalar UDF paths | googlesqlite | goccy/googlesqlite#94 |
| NUMERIC / BIGNUMERIC division rounds to scale 9 / 38 | googlesqlite | goccy/googlesqlite#95 |
| temp tables cleaned up when a script fails (QueryContext) | googlesqlite | goccy/googlesqlite#96 |
| APPROX_QUANTILES sorts and ignores NULLs; IGNORE NULLS no longer panics | googlesqlite | goccy/googlesqlite#97 |
| NUMERIC / BIGNUMERIC multiplication rounds to scale | googlesqlite | goccy/googlesqlite#98 (stacked on #95) |
| FORMAT %T literal forms (NUMERIC, JSON, INTERVAL, STRUCT, DATETIME) | googlesqlite | goccy/googlesqlite#99 |
| GROUP BY on STRUCT and ARRAY keys (constructor form stays permissive) | googlesqlite | goccy/googlesqlite#100 (stacked on #51) |
| aggregate over an ANY TABLE TVF (column ID collision) | googlesqlite | goccy/googlesqlite#101 (stacked on #88) |
| nested subquery column IDs in SQL UDF bodies | googlesqlite | goccy/googlesqlite#102 |
| IS [NOT] TRUE / FALSE never NULL | googlesqlite | third-party goccy/googlesqlite#71 (carried as is) |
| FLOAT64 text rendering (CAST, CONCAT, FORMAT, TO_JSON_STRING) | googlesqlite | third-party goccy/googlesqlite#58 (carried as is) |
| the HTTP server no longer fails queued requests with `read tcp ...: i/o timeout` (400 on jobs.insert / jobs.query with several concurrent dbt runs, Flip-to/dbt-virtual#34): requests are serialized by a mutex and `ReadTimeout` (15 s) / `WriteTimeout` (5 min) count from connection accept, so a request that waited in the queue could no longer read its body; now only `ReadHeaderTimeout` (15 s) is set. Regression test `TestQueuedRequestSurvivesReadDeadline` (`server/read_timeout_internal_test.go`) | emulator | none |
| query jobs created by jobs.insert run in the background (state RUNNING when the query outlives 5 s) and inserting an existing job ID is BigQuery's 409 `duplicate` / `ALREADY_EXISTS` (`Already Exists: Job <project>:<location>.<id>`) instead of 400 `failed to add job`. The Python client retries a timed-out jobs.insert with the same job ID and recovers from the 409 through jobs.get; jobs.get, jobs.getQueryResults (`timeoutMs`, then `jobComplete=false`) and a repeated insert of a running job are answered without waiting for the query. Regression tests in `server/jobs_async_internal_test.go` | emulator | none |

## Keeping the fork current

1. `git fetch upstream` and rebase each `fix/*` branch onto upstream `main`.
2. Drop a fix once upstream merges it or an equivalent (for the overlaps, prefer
   the upstream version when it lands).
3. Rebuild `flipto/main` by merging the remaining `fix/*` branches onto upstream
   `main`, run `go test ./...`, and bump the `replace` pseudo-version here.
