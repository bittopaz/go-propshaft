# go-propshaft

A framework-independent Go asset pipeline inspired by [Rails Propshaft](https://github.com/rails/propshaft). Resolve fingerprinted asset URLs, rewrite CSS dependencies, serve assets with `net/http`, or precompile them for a static server/CDN. No Node, bundler, or transpiler is required.

Module: `github.com/bittopaz/go-propshaft` · Package: `propshaft` · Go: **1.26.2+** · External dependencies: **none**.

This is the initial v0.1 implementation, not a drop-in Rails port. See [GitHub releases](https://github.com/bittopaz/go-propshaft/releases) for tagged versions.

## Run the example

From the repository root:

```sh
go run ./examples/basic       # immutable, embedded assets
go run ./examples/basic -dev  # refresh assets from disk on demand
```

Visit `http://127.0.0.1:8080`. The page uses an imported stylesheet, a CSS-referenced SVG, an image tag, and plain JavaScript. In development, edit the SVG or CSS and reload the page: affected URLs change automatically.

Build a standalone binary that does not need the asset source directory:

```sh
go build -o /tmp/propshaft-example ./examples/basic
/tmp/propshaft-example
```

## Library

```go
pipeline, err := propshaft.New(propshaft.Config{
    Roots:       []fs.FS{os.DirFS("assets"), os.DirFS("vendor/assets")},
    Prefix:      "/assets/", // default; also accepts an HTTP(S) CDN URL
    Development: true,
})
if err != nil {
    return err
}

mux.Handle("/assets/", pipeline.Handler()) // do NOT use http.StripPrefix

url, err := pipeline.URL("styles/app.css")
// /assets/styles/app-<64-character SHA-256>.css
```

For production, supply an `embed.FS` (use `fs.Sub` to select its asset directory) and leave `Development` false. `New` compiles all visible assets eagerly; after it returns, production pipelines no longer read the source filesystems.

The first root containing a logical path wins. Each root is a logical namespace: `styles/app.css` refers to that path under any root. Dotfiles and dot-directories are excluded. Regular files are copied unchanged except for supported CSS references; directories are traversed, and symlinks/special files are rejected. Roots must be trusted and readable. `os.DirFS` is not a security sandbox; use `os.OpenRoot(...).FS()` when disk containment matters, as the CLI and development example do. The caller owns and closes filesystem resources.

### Templates

The URL method is itself the template helper:

```go
tmpl, err := template.New("page").Funcs(template.FuncMap{
    "asset": pipeline.URL,
}).Parse(`<link rel="stylesheet" href="{{asset "styles/app.css"}}">`)
```

Missing assets return errors wrapping `fs.ErrNotExist`; invalid logical paths wrap `fs.ErrInvalid`. Template execution propagates lookup errors instead of rendering broken URLs. Buffer template output before sending the response, as the example does.

A logical path is a slash-separated filesystem name, not a URL: no leading slash, `.`/`..` segments, repeated separators, backslashes, or control characters. URL escaping is applied to the resulting asset URL. Append application query strings/fragments to the returned URL, not the lookup argument.

## Precompile CLI

Run from this checkout, or install locally with `go install ./cmd/propshaft`:

```sh
go run ./cmd/propshaft precompile \
  -root examples/basic/assets \
  -out /tmp/propshaft-build/assets
```

`-root` may be repeated; the first root wins. `-prefix /assets/` is the default. The prefix also identifies managed root-absolute CSS references during compilation.

Output contains fingerprinted files in their original directory structure and `manifest.json`:

```json
{
  "version": 1,
  "assets": {
    "app.js": "app-<full-sha256>.js"
  }
}
```

The digest placeholder above is illustrative; real manifests contain 64 lowercase hexadecimal characters. Filenames without extensions also receive fingerprints. The manifest records relative paths, not machine paths or deployment hosts.

**Output must not already exist**, even as an empty directory. The CLI rejects overlapping source/output directories, including aliases through symlinks. It compiles before writing, stages files beside the destination, and renames the completed staging directory into place. Failed writes clean up staging output. Use a new versioned directory for each build; deployment promotion and retention of older assets are your responsibility. Existing output is never intentionally overwritten or pruned.

The library also exposes `pipeline.Export(newDirectory)`. Unlike the CLI, it cannot inspect opaque `fs.FS` roots to detect overlapping directories; library callers must keep output outside their inputs. Output parents must be trusted, not concurrently controlled by another process. Staging is not a power-loss durability guarantee or an atomic replacement mechanism for a live directory.

### Serve precompiled output

The example supports this without source assets:

```sh
go run ./examples/basic -manifest /tmp/propshaft-build/assets/manifest.json
```

In an application, load the manifest once and use the same template pattern:

```go
file, err := os.Open("public/assets/manifest.json")
if err != nil {
    return err
}
defer file.Close()

manifest, err := propshaft.LoadManifest(file, "https://cdn.example.com/assets/")
if err != nil {
    return err
}
url, err := manifest.URL("styles/app.css")
```

Serve the output directory independently using a static server/CDN. Loading a manifest validates its version, paths, and fingerprint shape, not the file contents. Keep output immutable and publish it before deploying application code using the new manifest. Retain older output as needed for cached pages and rolling deployments.

Set `Cache-Control: public, max-age=31536000, immutable` for successful fingerprinted responses. Do not apply that policy to errors, the manifest, or application HTML. Disable directory listings if not needed. Static-server headers are your responsibility; the example's manifest mode deliberately uses a basic `http.FileServerFS`.

## Retain assets across releases

The CLI can merge **already-published** archives without recompiling or changing their bytes:

```sh
propshaft merge -manifest-from candidate -root previous -out prepared/candidate
propshaft merge -manifest-from previous -root candidate -out prepared/bridge
```

`-manifest-from` selects the manifest to preserve byte-for-byte and includes that archive's assets. Repeat `-root` for additional archives. All current and historical asset files are retained; differing bytes at the same path are an error. Native manifest fingerprints are checked against their file contents, and every referenced file must exist. Symlinks, special files, overlapping input/output directories, and existing output are rejected. Hidden files other than the historical root `.manifest.json` are ignored. Output is staged and published only after validation; source archives are never modified.

The merge command accepts native `manifest.json` and historical Ruby Propshaft `.manifest.json` archives. It does not convert their manifest formats or require Ruby. For an initial Vite migration, add **`-legacy-vite` to both commands**: this accepts `index.html` plus `assets/` and includes both root and `assets/` aliases, retaining older HTTP lookup layouts. Only the selected archive's manifest (or legacy index) is published; other releases' metadata is not copied as asset data.

This prepares artifacts, not a deployment. Applications must select and roll out the correct application/manifest pair. A rolling pool typically needs the old application with the merged **bridge** archive everywhere before deploying the new application with the **candidate** archive. Roll back to the prepared bridge, not to an old image missing newer assets. Archive retention and pruning remain operator decisions.

Library consumers can enumerate a validated manifest with `manifest.Entries()`, which returns an independent map of logical names to relative fingerprinted paths.

## CSS behavior

Compilation uses a small scanner rather than regex substitution. It recognizes:

- Quoted/unquoted `url(...)`, including escaped and case-insensitive function names.
- Quoted `@import "theme.css"` and `@import url(theme.css)`; import conditions/modifiers remain unchanged.
- CSS escapes, quoted-string line continuations, and comments.
- Relative references resolved from the containing stylesheet's directory.
- Root-absolute references **beneath the configured prefix**: with `/assets/`, `/assets/images/logo.svg` is managed, but `/uploads/photo.jpg` and `/images/logo.svg` are not.
- Percent-encoded filenames and query/fragment suffixes.

Managed references are rewritten to relative, URL-escaped fingerprinted paths. A stylesheet can move with the entire output tree to a different mount point/CDN without recompilation. Absolute references outside the managed namespace remain absolute.

External URLs, protocol-relative URLs, data/blob URLs, fragment-only references (including a leading `%23`), query-only references, and empty references are left unchanged. Ordinary strings and comments are not rewritten.

The SHA-256 fingerprint covers the **final served bytes**. Changing a referenced image changes its URL, the containing CSS bytes and URL, and all importing CSS URLs. Unrelated assets retain their URLs. Missing managed dependencies and dependency cycles fail compilation with context. Identical inputs and configuration yield identical output bytes and manifests.

### Deliberate limits and Rails differences

- Functional equivalence for the supported workflows, **not full Rails parity**. Rails digests and manifests are not compatible.
- Missing managed references fail instead of warning and leaving broken paths. Cyclic dependencies are rejected rather than supported.
- CSS must be UTF-8. The scanner is not a full stylesheet validator; malformed recognized strings/comments/URLs fail compilation.
- No rewriting of bare string URLs in `image-set(...)`, dynamic CSS expressions, or other resource-bearing syntax outside `url(...)` and `@import`. Use `url(...)` for managed resources.
- JavaScript is passed through unchanged. **Relative ES-module imports are not rewritten** and may not resolve in a fingerprint-only export. Use standalone scripts or explicitly managed module URLs/import maps; arbitrary module trees are not supported automatically.
- No Sass/PostCSS, minification, bundling, transpilation, SRI, source-map rewriting, pre-digested-asset recognition, or import-map management in v0.1.
- No filesystem watchers. Development rescans and compares every visible file on every URL lookup and asset request. This favors correctness over performance for small asset trees.
- Development publishes only successful compiled snapshots and reports rebuild errors. It does not retain previous generations: old fingerprint URLs return 404, never newer bytes. Reload a page after editing assets.
- Source filesystems do not provide transactional reads. Publish source changes atomically when necessary; compilation guarantees its own internally consistent bytes/dependencies, not a simultaneous snapshot across external file edits.
- Assets are held in memory. This is an application-asset pipeline, not a large-media streaming service.

## HTTP contract

`pipeline.Handler()` expects the original path beneath `Config.Prefix` (only the URL path is used for a CDN prefix). It serves known fingerprints only, with no directory listings, logical-path aliases, or stale-digest fallback. GET, HEAD, ETags, conditional requests, and byte ranges are supported.

Production success responses have year-long immutable caching. Development and error responses use `no-store`. Content types follow Go's MIME handling, with `X-Content-Type-Options: nosniff`. Modification-time headers are omitted because source timestamps do not define compiled content freshness. Pipeline and manifest methods support concurrent callers; callers must synchronize any mutable in-memory source filesystems they supply.

## Verification

```sh
go test ./...
go test -race ./...
go vet -tags=integration ./...
go test -tags=integration -run TestBinaries -v .
go test -run '^$' -fuzz '^FuzzCSS$' -fuzztime=20s .
go test -run '^$' -fuzz '^FuzzLogicalPaths$' -fuzztime=20s .
```

See [verification notes](docs/verification.md) for the checks actually run on this implementation.

## Upstream reference

Behavior was checked against [Rails Propshaft at dc979db](https://github.com/rails/propshaft/tree/dc979db89cd07c72ee4d11d415ae1cb4fd072623), especially its [CSS compiler](https://github.com/rails/propshaft/blob/dc979db89cd07c72ee4d11d415ae1cb4fd072623/lib/propshaft/compiler/css_asset_urls.rb) and [asset implementation](https://github.com/rails/propshaft/blob/dc979db89cd07c72ee4d11d415ae1cb4fd072623/lib/propshaft/asset.rb). This is an independent Go implementation; no upstream source code or fixtures were copied.
