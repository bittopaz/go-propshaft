# Initial implementation verification

Verified locally on **macOS arm64** with **Go 1.27.1** and the declared minimum toolchain, **Go 1.26.4**. All checks below passed. No subagents, external services, frontend build tools, or runtime dependencies were used for verification.

## Automated checks

| Check | Result |
| --- | --- |
| `go test -race -count=1 -cover ./...` | Passed; library statement coverage **91.3%**, CLI **84.9%** |
| `go test -race -tags=integration -count=1 ./...` | Passed, including binary integration tests (child binaries are built normally) |
| `go vet -tags=integration ./...` | Passed, including integration-test code |
| `GOTOOLCHAIN=go1.26.4 go test -race -count=1 ./...` | Passed on the minimum declared toolchain |
| `go test -tags=integration -run TestBinaries -v .` | Actual CLI/example binaries passed end-to-end checks on Go 1.27.1 |
| `GOTOOLCHAIN=go1.26.4 go test -tags=integration -run TestBinaries -count=1 -v .` | Same binary checks passed on Go 1.26.4 |
| `go test -run '^$' -fuzz '^FuzzCSS$' -fuzztime=20s -parallel=4` | Passed; approximately **210,000** executions |
| `go test -run '^$' -fuzz '^FuzzLogicalPaths$' -fuzztime=20s -parallel=4` | Passed; approximately **405,000** executions |
| `gofmt -l .` | No unformatted Go files |
| `go mod tidy`, then `go list -m all` | Only this module; no external dependencies or `go.sum` |

The example has no direct unit-test coverage; it is exercised as a real compiled process by the opt-in binary integration test. Those subprocess executions do not contribute to the statement-coverage percentages above.

## Verified behavior

- Changing an image invalidates its stylesheet and importing stylesheet; unrelated assets retain their URLs.
- Fingerprints match SHA-256 of the final HTTP response/exported bytes, including rewritten CSS.
- First-root precedence, ignored dotfiles/directories, extensionless and escaped filenames.
- Quoted/unquoted CSS URLs, imports and modifiers, CSS escapes, comments, queries/fragments, and unmanaged references.
- Actionable failures for missing dependencies, cycles, malformed recognized CSS constructs, invalid paths, symlinks, and special files.
- Production snapshots remain unchanged after source edits.
- Development detects same-size/same-timestamp changes, additions, removals, and recovery from failed compilation.
- Concurrent URL lookups and HTTP requests, including atomic disk replacements, do not race or serve bytes under the wrong fingerprint.
- GET, HEAD, ranges, conditional requests, content types, `nosniff`, stale URLs, and traversal rejection.
- Error responses do not retain immutable cache headers. Verification found and fixed a regression in HTTP `412`/`416` responses; both now have tests.
- Deterministic exports; manifest loading, validation, and relocation under another URL prefix/CDN host.
- Existing output is refused; source/output overlap and symlink aliases are rejected by the CLI; failed exports leave no published partial directory or staging directory.
- Template helper errors propagate instead of rendering broken asset URLs.

## Actual-binary deployment checks

[`integration_test.go`](../integration_test.go) builds the CLI and example and starts each server on a private loopback port. It checks:

1. **Embedded production:** the binary runs from an empty working directory without disk assets.
2. **Development:** modifying an SVG changes the rendered asset URLs; the obsolete stylesheet URL returns `404` with `no-store`.
3. **Precompiled production:** the CLI exports assets, refuses an overwrite, and the manifest-based example runs after the original source directory is moved away.
4. In every mode, HTML asset URLs are fetched, CSS dependencies are followed recursively, and every served asset's bytes are checked against its fingerprint.

Each test-owned process is stopped and temporary build/output directories are cleaned up by the test harness.

## Limits of this verification

This is bounded validation, not a claim of complete Rails parity, a formal security audit, or a proof that every CSS syntax is supported. Fuzzing was time-bounded. Browser rendering and JavaScript execution were not automated; the integration checks cover generated HTML, asset delivery, references, and byte integrity. Other operating systems were not executed. See the [documented v0.1 limitations](../README.md#deliberate-limits-and-rails-differences).
