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

## Carried changes

| change | repo | upstream |
|---|---|---|
| statement type and CTAS destination on query jobs | emulator | goccy/bigquery-emulator#518 (fixes #370) |
| CREATE OR REPLACE replaces an existing table | emulator | goccy/bigquery-emulator#519 |
| anonymous result columns named f0_, f1_, ... | emulator | goccy/bigquery-emulator#520 |
| nested wire format: anonymous STRUCT fields named _field_N, nested TIMESTAMP as epoch seconds, NULL arrays as [] | emulator | fork only |
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

## Keeping the fork current

1. `git fetch upstream` and rebase each `fix/*` branch onto upstream `main`.
2. Drop a fix once upstream merges it or an equivalent (for the overlaps, prefer
   the upstream version when it lands).
3. Rebuild `flipto/main` by merging the remaining `fix/*` branches onto upstream
   `main`, run `go test ./...`, and bump the `replace` pseudo-version here.
