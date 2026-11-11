# Insert audit

The insert tests use generated Prometheus protobuf requests and `httptest`
servers; no running ClickHouse is required. They exercise Snappy remote write,
RowBinary headers and rows, malformed input, writer failures, connection closing,
and label/ID stability.

## Known limitations retained for compatibility

### Series ID collisions

`id.NameWithSha256.Update` hashes sorted `name=value` pairs joined by `&`, without
escaping or length framing. These distinct, valid label sets have the same ID:

```text
{__name__="m", a="b&c=d"}
{__name__="m", a="b", c="d"}
```

Both hash `__name__=m&a=b&c=d`. This is an ambiguous preimage, not a SHA-256
collision. It can mix samples from different series. The executable reproducer
is `TestNameWithSha256KnownDelimiterCollision` in
`id/name_with_sha256_regression_test.go`.

The production ID algorithm is deliberately unchanged. Even escaping only the
separators would change IDs of existing series with those characters. A fix
needs a versioned ID format and an explicit historical-data strategy: backfill
IDs from original labels where recoverable, migrate associated sample/series
tables, or support old and new IDs during a transition. Samples already merged
under an ambiguous ID may be impossible to separate without original data.

Duplicate label names are invalid Prometheus input, but are currently accepted;
their order can affect the ID. This is documented by
`TestNameWithSha256KnownDuplicateLabelOrderDependence`, not silently changed.

### Request size and memory

`remote_write.go` still reads the whole compressed body with `io.ReadAll` and
decodes Snappy without a configured decompressed-size limit. Large requests or
compression bombs can exhaust memory. Both compressed and decoded limits, and
sample/label limits, should be designed as a separate configuration change.
The new wire validation checks correctness, not overall resource budgets.
RowBinary label readers no longer allocate based solely on untrusted counts or
string lengths, but naturally still consume memory proportional to actual data.

### ID function configuration

`Config.Insert.IDFunc` and insert overrides are not consulted by the HTTP
receiver: it constructs `id.NewNameWithSha256()` directly. The base validator
currently permits only `name_with_sha256`; an override permits an empty string
or that same name, and empty overrides inherit the base value. There is no
second valid algorithm to select, so the option is redundant rather than a
currently testable alternative implementation.

### Upstream request lifecycle

Malformed protobuf is rejected with HTTP 400 before opening `query.Request`.
This avoids the existing query-layer cancellation/pipe-close deadlock on that
path. `TestPrometheusRemoteWriteUpstreamWriteError` independently exercises an
early upstream response during streaming and requires HTTP 502 without waiting
for its context deadline. This audit does not replace the separate query-layer
pipe and response-body lifecycle fixes.
