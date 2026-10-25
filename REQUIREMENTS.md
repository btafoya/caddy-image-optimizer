# Caddy Image Optimizer: Requirements (brainstorm output)

Status: requirements discovery only. No design or implementation decisions beyond those listed.

## Goal

An external Caddy plugin (built with xcaddy, separate Go module) that serves resized and re-encoded images on demand from a local file_server root, with a disk cache.

## Findings from Caddy source review

- Core has no image handling. `http.handlers.encode` only does Content-Encoding (text-only default matcher, min length 512).
- `file_server` supports precompressed sidecars (`staticfiles.go`), selected by Accept-Encoding. Etag is derived from the sidecar file.
- Plugin points: `http.handlers.*` middleware module wrapping the ResponseWriter (as `encode` does), or sitting before `file_server` in the route chain.
- Caddy does not set `Vary: Accept` or handle etags for image variants; the plugin must.
- `intercept` is experimental; not needed for the local-root scope.

## Decisions

| Topic | Decision |
|---|---|
| Form | External xcaddy plugin in a fresh repo on current Caddy/Go. Survey: ueffel/caddy-imagefilter has no WebP output and no cache; caddy-pixbooster is archived; quix-labs/caddy-image-processor is a ~900-line prototype (last commit 2024-12-15, Caddy 2.8.4 / Go 1.22, zero tests, no cache, unbounded params, deletes Vary, double-writes on abort). Borrow its MIT config code (SecurityOptions, constraints, Caddyfile parsing) with attribution; the request path is a rewrite. |
| Codec | libvips via CGO only, using govips (davidbyttow/govips). No pure-Go fallback. |
| Naming | Module ID `http.handlers.image_optimizer` (avoids clash with quix-labs `image_processor`). |
| Timing | On-demand transform plus disk cache |
| Source | Local file_server root only (reverse_proxy and remote fetch out of scope for v1) |
| Request API | Query params (`w`, `h`, `q`, `fmt`), Accept-header negotiation, and named presets |
| Output formats v1 | WebP, JPEG, PNG. AVIF deferred to v1.1; format layer must allow it as a drop-in. |
| Abuse protection | Configurable allowlists plus hard caps |
| Cache | Local directory, size cap, LRU eviction |

## Functional requirements

1. Match image requests (by extension/content type) under the configured root; non-images pass through untouched.
2. Accept `w`, `h`, `q`, `fmt` query params and a `preset` param mapping to a configured set.
3. When `fmt` is absent, negotiate output from the Accept header (WebP if accepted, else original format).
4. Resize by scale-to-fit only, preserving aspect ratio. No crop/cover/pad in v1.
5. Cache transformed output on disk keyed by source path, source mtime/size, and normalized params. Source change invalidates.
6. Set `Vary: Accept`, correct `Content-Type`, `Content-Length`, a variant-specific `ETag`, and support conditional requests (304) and range-free full responses.
7. Strip metadata (EXIF) by default; keep orientation correct (apply EXIF rotation before stripping).
8. Provide a Caddyfile directive and JSON config.
9. Animated sources (GIF, APNG, animated WebP) pass through unchanged.
10. Disallowed param values handled per `on_disallowed`: reject (400, default), snap to nearest allowed, or bypass to original.
11. `presets_only` switch (default off) disables raw `w/h/q`.
12. `cache_control` option overrides Cache-Control on transformed responses only; otherwise source headers pass through.

## Non-functional requirements

- Safety: allowlists for widths, qualities, and formats; max source pixels (decompression-bomb guard); max concurrent transforms; per-transform timeout; cache size cap with LRU eviction. All configurable, safe defaults.
- Path safety: no traversal outside the root; reuse file_server's root and path-sanitizing semantics.
- Singleflight: concurrent identical requests produce one transform.
- Stampede/cold cache must not block unrelated requests.
- Build: default build is pure Go and works with plain `xcaddy build --with ...`.
- Observability: Caddy structured logging; metrics (hits, misses, transform time) are a nice-to-have.
- Tests: golden-image tests per codec backend, cache-key tests, limit/abuse tests, Caddyfile adapt tests.

## Acceptance criteria (sample)

- `GET /a.jpg?w=400` returns a 400px-wide JPEG; second request is served from cache without re-encoding.
- `GET /a.jpg` with `Accept: image/webp` returns WebP with `Vary: Accept`.
- `GET /a.jpg?w=99999` is rejected (400) or clamped per config; the cache gains no entry.
- A source image exceeding the max-pixel cap returns an error and does not allocate the decoded bitmap.
- Replacing `a.jpg` on disk causes the next request to regenerate.
- Cache exceeding the size cap evicts least-recently-used entries.
- `If-None-Match` with the variant's etag returns 304.

## Resolved questions

- Fit modes: scale-to-fit only, aspect ratio preserved, no crop/cover/pad in v1.
- Disallowed width/quality: configurable per site (`on_disallowed`: reject 400 | snap to nearest | bypass original). Default reject.
- `presets_only` config switch disables raw `w/h/q`; off by default.
- Animated GIF/APNG/WebP sources: pass through unchanged.
- Cache-Control: pass through source headers; optional `cache_control` override applied to transformed responses only.
- govips health: v2.20.0 released 2026-10-16, monthly releases, 4 open issues, 1681 stars, not archived. Healthy.

## Constraints carried into design

- govips: `Startup` once per process, never `Shutdown` (Caddy reloads re-provision modules). libvips >= 8.10. Verify header-only dimension read for the max-pixels guard.
- Directive placement: handler runs before `file_server` in the route chain; own it as a middleware that serves from the file_server root.

## Open questions (remaining)

1. Cache eviction: LRU by access time needs bookkeeping on a disk dir. Likely an in-memory index rebuilt at startup; confirm acceptable startup scan cost.
2. Standalone handler before `file_server` vs integrating with `file_server` itself. Standalone is implied by the external-plugin decision.
3. Caddy version support floor (current main requires Go 1.26).

## Next step

`/sc:design` for module layout, the cache design, and the request flow.
