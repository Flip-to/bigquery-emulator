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
| statement type and CTAS destination on query jobs | emulator | PR pending (fixes #370) |
| CREATE OR REPLACE replaces an existing table | emulator | PR pending |
| LEFT JOIN UNNEST keeps empty/NULL-array rows | googlesqlite | PR pending |
| SPLIT(NULL) is a NULL array | googlesqlite | PR pending |
| TVF handle kept alive (GC use-after-free) | googlesqlite | PR pending |
| ANY TABLE / TABLE<...> TVF parameters | googlesqlite | PR pending |
| quoted dotted prefix in table/TVF paths | googlesqlite | PR pending |
| DROP TABLE FUNCTION | googlesqlite | PR pending |
| GROUP BY ALL | googlesqlite | overlaps goccy/googlesqlite#51 |
| MOD returns INT64 | googlesqlite | overlaps goccy/googlesqlite#60 |
| ARRAY_TO_STRING NULL | googlesqlite | overlaps goccy/googlesqlite#56 |
| comments in the DECLARE/SET pre-pass | googlesqlite | overlaps goccy/googlesqlite#55 |
| sub-catalogs without builtins (memory growth per DROP) | googlesqlite | overlaps goccy/googlesqlite#80 |
| LIKE `_` and backslash escapes | googlesqlite | overlaps goccy/googlesqlite#67 |

## Keeping the fork current

1. `git fetch upstream` and rebase each `fix/*` branch onto upstream `main`.
2. Drop a fix once upstream merges it or an equivalent (for the overlaps, prefer
   the upstream version when it lands).
3. Rebuild `flipto/main` by merging the remaining `fix/*` branches onto upstream
   `main`, run `go test ./...`, and bump the `replace` pseudo-version here.
