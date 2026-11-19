# Pluto: repository knowledge for coding agents

This file records what was learned during the project audit. **Check the current
branch before applying any advice below.** The eight audit branches were merged
into `renew` at `04ff2f5` with the user's approval. The `audit/*` branches
are kept for provenance; their worktrees have been removed. This `AGENTS.md`
and the `.gitignore` entry for `.codegraph/` were committed after the merge, in
`de12423`.

## Purpose and architecture

- Pluto is a Go 1.26 module (`github.com/lomik/pluto`) implementing a Prometheus
  remote-write receiver and Prometheus-compatible read/UI endpoints on top of
  ClickHouse. `cmd/pluto/main.go` is the executable entry point.
- `pkg/config` loads YAML with `configor`, validates it, compiles `expr` `when`
  expressions, and chooses the first matching `override_insert`,
  `override_series`, or `override_samples`. Insert expressions can inspect
  request query parameters (`GET`) and headers (`HEADER`); series/sample
  expressions can inspect query hints. `config.yaml` has the main settings;
  `example/*/config.yaml` illustrates alternative setups.
- `pkg/insert` handles `/api/v1/write`: read a Snappy-compressed protobuf
  request, convert series and samples to ClickHouse
  `RowBinaryWithNamesAndTypes`, and stream an INSERT to ClickHouse. `pkg/insert/id`
  generates a name plus SHA-256 series ID after sorting labels;
  `pkg/insert/labels` serializes label maps.
- `pkg/query` is the streaming HTTP/`io.Pipe` ClickHouse client, handling DSN
  parameters, headers, credentials, optional discovery, response bodies, and
  cancellation. Call sites in `insert` and `prom` depend on its lifecycle.
- `pkg/prom` implements the Prometheus storage/query interfaces and API/UI.
  Queries select series and labels, then request samples by ID using a
  multipart ClickHouse query with an external `ids` table. RowBinary readers
  decode responses; a 32-bit ID hash is used when collision-free for the
  selected IDs, otherwise full IDs are used. SQL is assembled with `pkg/sql`.
  `pkg/where` is a separate Grafana-style glob/matcher-to-SQL package; it is
  not currently imported by the executable or `pkg/prom`, so its test and
  behavior changes should not be mistaken for changes to PromQL label matching.
- `pkg/listen` maps listen addresses to `http.ServeMux` instances so insert,
  debug, and Prometheus routes can share an address. `pkg/lg` supplies logging
  helpers; `pkg/errs` supplies coded errors.
- Typical configuration uses `0.0.0.0:9095` for insert and debug routes and
  `0.0.0.0:9096` for the Prometheus UI/API. Debug routes include `/metrics`
  and `/debug/pprof/`; the write route is `/api/v1/write`. Prometheus API/UI
  routes may have a configured `route_prefix`.
- `example/simple/init.sql` defines `samples_null`, `samples`, materialized
  views, and `series`; timestamps in the default schema are milliseconds and
  series/samples are partitioned by day. `example/sharding` shows separate
  ClickHouse databases selected by insert overrides and merged read tables.
  `example/downsampling` has a `samples_1h` materialized view and a sample
  table override: downsampling requires the example schema/configuration; it
  is not performed automatically by the Go process alone.

## Building, running, and testing

```sh
go build -o pluto ./cmd/pluto
go test ./...
go test -race ./...
go vet -stdmethods=false ./...
```

- `go.mod` requires Go 1.26.7 (raised by `github.com/prometheus/prometheus`
  v0.316.0; earlier raised from 1.24.0 to 1.25.0 by `github.com/lomik/rawpb`
  v1.3.0; `rawpb` and `rowbinary` moved from `github.com/pluto-metrics/*` to
  `github.com/lomik/*`, and rowbinary v0.0.2 folded the `schema` package into
  the root: `FormatReader`/`FormatWriter`, `Scan` instead of `Read`). The audit was run locally on Go 1.27.1 on
  macOS/arm64. `go vet ./...` on the baseline reports a `stdmethods` warning
  about `pkg/prom/series_set.go`'s `Seek(int64) chunkenc.ValueType`; that
  signature is required by the upstream Prometheus iterator interface.
  Disabling only `stdmethods` lets the remaining vet checks run.
- Before the audit, `go test ./...` and `go test -race ./...` passed without a
  running ClickHouse, but overall statement coverage was only 24.0%. `cmd/pluto`,
  `pkg/config`, `pkg/insert`, `pkg/insert/id`, `pkg/insert/labels`, and
  `pkg/listen` had no effective tests; `pkg/prom` was at 0.6%. `pkg/query` was
  at 90.2%, `pkg/sql` at 96.2%, `pkg/where` at 63.4%, and `pkg/errs` and
  `pkg/lg` at 100% before the audit branches.
- The audit introduced `httptest`-based ClickHouse/protocol tests, subprocess
  tests, race checks, and some fuzz tests. On the separate branch HEADs before
  merging, reported package coverage was: config 92.0%, insert 91.1%,
  insert/id 97.8%, insert/labels 97.6%, prom 81.8%, query 93.5%, listen
  94.7%, where 87.6%, and cmd/pluto 97.6%. These are *branch-local* figures;
  no trustworthy combined coverage number was obtained.
- A read-only Go overlay of all eight final audit HEADs passed
  `go test -mod=readonly -race -count=3 -shuffle=on -timeout=120s ./...`
  (12 packages) and `go vet -mod=readonly -stdmethods=false ./...`. All 41
  changed paths were non-overlapping. The same three-repeat race suite, vet,
  and one-file Linux/Windows cross-builds passed on the actual merge commit
  before `renew` was fast-forwarded to it.
- There was no real ClickHouse integration test, actual GitHub release run,
  completed Docker image build, or Windows runtime test. Windows test packages
  were cross-compiled. SQL semantics and ClickHouse-specific regex/LIKE
  escaping still merit checks against a live ClickHouse instance.
- `Dockerfile` builds **`cmd/pluto/main.go` by filename**, not `./cmd/pluto`:
  adding another production `.go` file under `cmd/pluto` without changing
  that Dockerfile silently excludes it from this image. GoReleaser instead
  builds `./cmd/pluto` and uses `Dockerfile.goreleaser` for images; its config
  includes Linux, macOS, and Windows builds. Before the audit, the tag-only
  workflow installed Go 1.23.2 despite `go.mod` requiring 1.24 and lacked a
  test gate; the merged workflow uses the `go.mod` version and runs
  `go test ./...` before release. The GoReleaser `before` hook also runs
  `go mod tidy`, which may change module files during a release.

## Audit branches merged into `renew`

All branches below started at `5cf62d4`, had distinct file ownership, and were
merged into `renew` in octopus merge `04ff2f5`. These are the source branch
HEADs; branch names are retained for provenance, not a pending merge list.

| Branch | Final HEAD | Scope and findings |
| --- | --- | --- |
| `audit/config-tests` | `6a3876b` | `pkg/config`: YAML/defaults/overrides tests; validate nested overrides, allow inherited override DSN while requiring a base DSN, reject a missing config file. |
| `audit/insert-tests` | `092568c` | `pkg/insert`: HTTP/Snappy/protobuf/RowBinary tests and fuzzing; reject malformed protobuf before opening upstream, avoid unsafe RowBinary allocations, fix `CloseConnections` EOF and writer error propagation. See `pkg/insert/AUDIT.md`. |
| `audit/prom-tests` | `595dd41` | `pkg/prom`: fake ClickHouse and remote-read tests; handle nil hints, bad RowBinary/IDs, sorting, label-key collisions and small UInt32 steps; configure remote-read concurrency; reject unsupported streamed chunk reads with an explicit error instead of a panic. |
| `audit/query-tests` | `94b4ca5` | `pkg/query`: HTTP lifecycle tests; apply discovered DSN, close response bodies, unblock failed uploads, preserve valid full-duplex uploads after early HTTP 200. |
| `audit/listen-tests` | `ce42b1c` | `pkg/listen`: lifecycle/concurrency tests; avoid empty-listener hangs and a long-held `Mux` lock, release listeners on bind errors. |
| `audit/where-tests` | `9884750` | `pkg/where`: glob/LIKE/SQL escaping, invalid-input, table-driven and fuzz/property tests. This deliberately treats glob brace alternatives as glob patterns, not embedded regex; see compatibility caveat below. |
| `audit/cmd-tests` | `27c7adb` | `cmd/pluto`: route/subprocess tests and signal-aware clean exit. Production changes stay in `main.go` to preserve the current Docker build. See `cmd/pluto/TESTING.md`. |
| `audit/ci-tests` | `8b7bd2c` | `.github/workflows/goreleaser.yml`: choose Go version from `go.mod`, run `go test ./...` before publishing. |

Independent reviewers found and prompted follow-up commits for a full-duplex
upload regression in `query`, non-repeatable global-registry tests in `prom`,
Windows-specific test assertions in `cmd`/`listen`, and a streamed remote-read
panic in `prom`. The final HEADs above include those follow-ups. After the
user authorized merging, the combined tree passed the full race suite and vet.

## Accepted risks and outstanding decisions

- **Persisted series-ID format:** `NameWithSha256` hashes sorted
  `name=value` pairs joined with `&` without escaping/length framing.
  `{__name__="m", a="b&c=d"}` and `{__name__="m", a="b", c="d"}` produce
  the same ID, mixing distinct series. This is an ambiguous encoding, not a
  cryptographic SHA-256 collision. **Accepted risk: the user decided to leave
  the existing ID algorithm unchanged because this is unlikely in real-world
  use. Do not reopen the format, escaping, or migration discussion unless an
  actual collision is observed or the user explicitly requests it.** Keep the
  reproducing test and documentation. Duplicate label names can also lead to
  order-dependent IDs. The receiver constructs the SHA-256 provider directly,
  even though `id_func` is exposed in configuration; currently no other valid
  base provider is configured.
- **Resource limits:** the receiver reads the entire compressed request and
  Snappy-decodes without compressed/decoded-size limits; a large payload or
  compression bomb can exhaust memory. Wire validation is not a size limit.
- **Remote read:** before the audit, `storageImpl.ChunkQuerier` returned
  `(nil, nil)`, so a streamed XOR-chunks request with a query could panic. It
  now returns an error instead: the server no longer crashes, but chunked
  remote read still responds HTTP 500. Implement actual chunk support or
  choose/advertise only supported response formats if streamed clients must
  work. The storage implementation also returns `nil`
  from `Appender`; do not assume it is a general-purpose writable Prometheus
  storage merely because it satisfies the storage interface.
- **Override semantics:** `clickhouse.params` overrides replace the entire
  map rather than merging keys; `{}` can deliberately clear base params.
  Decide the intended contract before changing it. `mergeZero` cannot use
  an override value of `false` to turn a base
  `select.samples_timestamp_uint32: true` off. These issues predate the audit.
- **Glob compatibility:** the merged `pkg/where` change quotes regex
  metacharacters within glob alternatives, matching glob semantics. Someone
  relying on the old accidental regex interpretation of `foo.{a,b+}` or
  nested parentheses will see different matches; communicate this behavior change to users or
  specify a separate regex-in-braces contract if compatibility is required.
- **Shutdown and CLI:** `pkg/listen` closes servers with `Close`, not graceful
  `Shutdown`, so in-flight requests are not awaited. `-dev` is parsed in
  `cmd/pluto/main.go` but does not currently change the config path; its
  intended behavior was not established. A successful end-to-end write or
  PromQL query needs a real ClickHouse-backed test.

When working here, prefer deterministic `httptest`/RowBinary regression tests
for every fix. Be careful with HTTP early responses versus completed uploads,
global Prometheus registries in repeatable tests, Windows-specific signals
and error text, and the compatibility of ClickHouse schemas and stored IDs.
