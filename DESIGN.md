# Caddy Image Optimizer: Design

Status: design for review. Input: REQUIREMENTS.md. No implementation yet.
Module path (assumed, confirm): `github.com/btafoya/caddy-image-optimizer`. Caddy module ID: `http.handlers.image_optimizer`. Caddyfile directive: `image_optimizer`.

## 1. Shape of the solution

One HTTP middleware. It does not wrap `file_server` and does not buffer a response. For a request that asks for a transform it opens the source file itself (same root and path rules as `file_server`), produces or finds a cached variant, and serves the variant with `http.ServeContent`. Everything else calls `next`, so `file_server` handles it.

Why not wrap `file_server` with a ResponseRecorder (the quix-labs approach): that reads the whole original into memory on every request, loses `ServeContent` features, and couples us to upstream headers. Reading the file ourselves lets libvips shrink-on-load straight from disk.

Verified in Caddy source:
- `caddyhttp.SanitizedPathJoin(root, reqPath)` exists and handles traversal.
- `file_server` defaults its root to `{http.vars.root}`; we use the same default.
- `http.ServeContent` already gives us 304, If-Modified-Since, and Range. `file_server` relies on it too.
- Directive order is `encode` < `templates` < `respond` < `file_server`. Registering `image_optimizer` "before file_server" puts it inside `encode`. Image types are not in encode's default matcher, so no double compression.

## 2. Package layout

```
caddy-image-optimizer/
  module.go        Middleware struct, CaddyModule, Provision, Validate, ServeHTTP
  caddyfile.go     directive registration and UnmarshalCaddyfile
  config.go        Limits, Allow, Preset, OnDisallowed types and defaults
  request.go       parse + validate query -> Variant (normalized, comparable)
  negotiate.go     Accept header -> output format
  sniff.go         magic-byte sniff, header-only dimensions, animation detection
  pipeline.go      govips thumbnail + export (the only file that imports govips)
  vips.go          package-level sync.Once Startup(cfg); never Shutdown
  cache.go         disk store, key hashing, atomic write
  lru.go           in-memory index, size accounting, eviction
  serve.go         headers, ETag, ServeContent from cache file
  internal/borrowed/   constraints/security config code from quix-labs (MIT, attributed)
```

Only `pipeline.go` and `vips.go` touch govips and CGO, so the rest is unit-testable without libvips installed. A `Pipeline` interface (`Transform(ctx, srcPath, Variant) (outPath-or-bytes, error)`) lets tests stub it.

## 3. Request flow

```
ServeHTTP
 1. No query params (w,h,q,fmt,preset) and Accept does not apply
      -> next
 2. Method not GET/HEAD -> next
 3. path := SanitizedPathJoin(root, r.URL.Path); stat
      not found / dir / not regular file -> next   (file_server owns 404s)
 4. Extension prefilter (.jpg .jpeg .png .webp); else -> next
 5. Sniff first 16 bytes. Not JPEG/PNG/WebP -> next (passthrough).
 6. Header-only decode: dimensions + animation flag
      animated -> next
      w*h > max_source_pixels or size > max_source_bytes -> 422 (or next, see on_limit)
 7. Parse query -> Variant{W,H,Q,Fmt}; apply preset; apply allowlists
      disallowed -> on_disallowed: reject 400 | snap | bypass(next)
      presets_only and raw w/h/q present -> same on_disallowed handling
 8. Resolve output format: fmt param, else negotiate from Accept
      (WebP if Accept lists image/webp; else source format)
 9. key := sha256(relpath | src mtime ns | src size | Variant | schemaVersion)
10. ETag := `"` + hex(key)[:32] + `"`. Deterministic, so If-None-Match -> 304
      is answered here without touching the cache or libvips.
11. Cache lookup (lru index). Hit -> serve (step 14).
12. Miss -> singleflight(key):
      acquire semaphore (ctx-aware, wait up to queue_timeout, else 503 + Retry-After)
      pipeline.Transform under transform_timeout, using a context detached from
      the first requester's cancellation
      write temp file in cache dir, fsync-free rename into place, add to lru index
13. Transform failure -> on_fail: bypass (next) | abort (500). Never write twice.
14. Serve: set Content-Type, ETag, Last-Modified (source mtime),
      Vary: Accept (Header().Add, only when format was negotiated),
      Cache-Control per config; http.ServeContent(cacheFile)
```

Steps 1 to 10 are cheap and allocate no pixel buffers, so abuse requests are rejected before any decode.

## 4. Variant and cache key

`Variant{W, H, Q int; Fmt Format}` after normalization: zero values mean "unset", `Q` defaults per format, `Fmt` is always concrete (negotiation resolved). The key includes source mtime and size, so replacing the file produces a new key and the old entry ages out via LRU (no explicit invalidation). `schemaVersion` is bumped when encoder defaults change.

Known tradeoff: requested `w` larger than the source width yields identical bytes under different keys, because upscaling is disabled in the pipeline (`SizeDown`). The allowlists and `max_width` bound the duplicates, and clamping before keying would need EXIF-orientation-aware dimensions. Not worth it in v1.

## 5. Format negotiation and inputs

- Inputs v1: JPEG, PNG, WebP (sniffed by magic bytes, not extension). Anything else, or any animated file, passes through to `file_server` unchanged.
- Outputs v1: WebP, JPEG, PNG. AVIF is v1.1: add an entry to the format table in `negotiate.go` and `pipeline.go`; nothing else changes.
- `fmt` explicit: use it (and do not add `Vary`).
- `fmt` absent: WebP if the client accepts it, else keep the source format; `Vary: Accept` added.
- Alpha to JPEG: flatten onto white.
- Metadata: stripped by default; orientation applied first (thumbnail autorotates; confirm in spike).

## 6. Pipeline (govips)

- `vips.go`: `sync.Once` calls `vips.Startup(&vips.Config{ConcurrencyLevel, MaxCacheMem, MaxCacheFiles})` from the first provisioned instance's config. Never `Shutdown` (Caddy reloads re-provision; govips cannot restart). Config on later reloads cannot change libvips settings; log a warning if they differ.
- Resize: `NewThumbnailWithSizeFromFile(path, w, h, InterestingNone, SizeDown)`. Loads from disk with shrink-on-load. If only `w` is set, pass a very large `h` (libvips thumbnail defaults height to width otherwise). `w` and `h` form a bounding box; aspect ratio is preserved.
- Export: `ExportJpeg{Quality, StripMetadata, Interlace}`, `ExportPng{Compression}`, `ExportWebp{Quality, ReductionEffort}`.
- `ImageRef.Close()` in `defer` on every path.
- Pixel guard uses Go `image.DecodeConfig` (stdlib JPEG/PNG, `x/image/webp`) for header-only dimensions. This does not depend on how lazily libvips opens files.
- Animation detection: WebP `VP8X` animation flag; PNG `acTL` chunk before `IDAT`.

## 7. Cache

- Layout: `<dir>/<aa>/<bb>/<hash>.<ext>` (two-level fan-out). Writes go to `<dir>/.tmp/` then `os.Rename` (atomic on the same filesystem).
- Index: in-memory map `key -> {size, lastAccess}` plus a list for LRU; rebuilt at startup by scanning the directory (access time = file mtime; `Chtimes` on hit throttled to once a minute so eviction survives restarts).
- Eviction: when total bytes exceed `max_size`, delete oldest until below 90 percent. Runs in a background goroutine, never on the request path.
- Startup scan cost is O(files); acceptable for the expected size (open question: confirm at 1M files).
- Crash safety: leftover `.tmp` files are deleted at startup.
- Cache directory is created `0700`. It must be on the same filesystem as `.tmp`.
- `Cleanup()` stops the eviction goroutine. A reload creates a new instance over the same directory; two instances briefly share it safely because writes are rename-atomic and eviction tolerates missing files.

## 8. Concurrency and limits

| Setting | Default | Purpose |
|---|---|---|
| `max_source_pixels` | 50,000,000 | decompression-bomb guard |
| `max_source_bytes` | 50 MiB | file size guard |
| `max_width` / `max_height` | 4096 | output cap when allowlists are off |
| `max_concurrent` | `runtime.NumCPU()` | semaphore around transforms |
| `queue_timeout` | 5s | wait for a slot, then 503 + `Retry-After` |
| `transform_timeout` | 15s | per-transform ceiling |
| `max_size` (cache) | 1 GiB | disk budget |

Singleflight (`golang.org/x/sync/singleflight`, `DoChan`) collapses identical concurrent misses. The work context is detached from the first requester (`context.WithoutCancel` plus the timeout) so one client disconnecting does not fail the others; each waiter still selects on its own `r.Context().Done()`.

## 9. Config surface

Caddyfile:

```
image_optimizer {
    root            {http.vars.root}
    cache           /var/cache/caddy/images   max_size 2GiB
    widths          320 640 960 1280 1920
    heights         0
    qualities       40-90
    formats         webp jpg png
    preset thumb    w=320  q=70
    preset hero     w=1920 q=80
    presets_only    off
    on_disallowed   reject          # reject | snap | bypass
    on_fail         bypass          # bypass | abort
    cache_control   "public, max-age=604800"
    limits {
        max_source_pixels 50000000
        max_concurrent    4
        transform_timeout 15s
    }
}
```

JSON mirrors this under `http.handlers.image_optimizer`. `snap` rounds a requested value up to the smallest allowed value that is at least as large, or the largest allowed if none is. Unlisted option keys fail `Validate()` with the key named. Build: `CGO_ENABLED=1 xcaddy build --with github.com/btafoya/caddy-image-optimizer`; libvips 8.10+ dev headers needed at build, libvips at runtime.

## 10. Failure behaviour

| Condition | Result |
|---|---|
| Not an image, animated, unsupported type | passthrough to `file_server` |
| File missing | passthrough (404 from `file_server`) |
| Over pixel or byte limit | 422 |
| Disallowed param | per `on_disallowed` |
| Queue wait exceeded | 503 + `Retry-After: 1` |
| Transform error or timeout | per `on_fail`; response written exactly once |
| Cache write error | serve the in-memory result once, log, do not fail the request |

## 11. Tests

- Pure-Go unit tests (no libvips): query parsing, allowlist/snap logic, key stability, Accept negotiation, sniff and animation detection, LRU eviction ordering, ETag 304 path with a stub `Pipeline`.
- libvips integration tests behind a build tag or skip-if-missing: golden dimensions (not byte-exact) for JPEG/PNG/WebP, alpha flatten, EXIF orientation, shrink-on-load memory sanity.
- Caddy-level: `caddytest` config adapt test and an end-to-end request through a real route with `file_server`.
- Concurrency: N identical requests produce one transform (counter in stub); semaphore saturation returns 503; client cancel does not break other waiters.
- Abuse: `w=99999`, huge-pixel fixture, truncated file, path traversal, non-image with image extension.

## 12. Spikes to run before implementing

1. govips `NewThumbnailWithSizeFromFile` with `SizeDown`: confirm EXIF autorotate, shrink-on-load, and the large-`h` trick on a real JPEG.
2. Confirm `x/image/webp.DecodeConfig` handles lossy and lossless WebP (and rejects VP8X-animated gracefully).
3. Measure startup scan time for 100k and 1M cache files.
4. Check whether govips can set `VIPS_BLOCK_UNTRUSTED`; if not, document the env var for Docker. The magic-byte sniff already restricts what reaches libvips to three formats.

## 13. Implementation stages

1. Skeleton module, config, Caddyfile adapt tests (no libvips).
2. Request parsing, allowlists, negotiation, sniff, key and ETag (no libvips).
3. Pipeline plus vips init, integration tests.
4. Disk cache, LRU, singleflight, semaphore.
5. Serve path, headers, Docker/xcaddy build docs, abuse tests.

## 13a. As built: deviations from this design

- Caddyfile uses separate `cache_dir` and `cache_max_size` directives (not `cache PATH max_size N`); `presets_only` is a flag with an optional `true|false` argument; `limits { ... }` is a nested block as designed.
- No `Pipeline` interface: a `transform` function field on the middleware is the test seam.
- Eviction runs inline on a cache put (a miss already paid for a transform), not in a background goroutine, so there is no `Cleanup`.
- A cache write error is treated as a transform failure (`on_fail`), not served from memory.
- Oversize source: explicit transform request returns 422; Accept-negotiation-only requests pass through to the original.
- libvips runs with one thread per transform; parallelism comes from `max_concurrent`.
- Spikes 1 and 2 are covered by tests (thumbnail autorotate with SizeDown, width-only box, `x/image/webp` header read). Spikes 3 (startup scan at scale) and 4 (`VIPS_BLOCK_UNTRUSTED`) remain open; the magic-byte sniff already limits libvips to JPEG/PNG/WebP.

## 14. Open items

- Confirm module path and Go module name.
- Metrics (hit/miss/transform time) deferred to v1.1; log fields at debug for now.
- Confirm default limit values (pixels 50M, 4096 cap, 1 GiB cache) suit the target sites.
