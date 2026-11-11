# Command smoke tests

`go test ./cmd/pluto` exercises route registration using in-memory HTTP muxes
and runs the command in a subprocess for listener errors, `-config` with `-dev`,
and SIGINT/SIGTERM. No ClickHouse service is needed: remote-write tests send
malformed input, and Prometheus tests use endpoints that do not query storage.

Limits of these tests and the command:

- Signal subprocess tests are skipped on Windows, where `os.Process.Signal`
  cannot send SIGINT or SIGTERM. Listener error tests still run on Windows.
- A successful remote write and a PromQL query require ClickHouse (or a
  substantial fake of its protocol) and are not part of these smoke tests.
- `-dev` is parsed but has no effect on configuration loading. Its intended
  default configuration path is not specified, so this branch leaves its
  behavior unchanged.
- On cancellation, `pkg/listen.HTTP.Run` currently closes HTTP servers with
  `Close`, not `Shutdown`: the command exits without waiting for in-flight
  requests. Fixing this requires a change outside `cmd/pluto`.
- The Dockerfile builds `cmd/pluto/main.go` by filename, so command code must
  remain in that file until the Dockerfile is changed to build the package.
