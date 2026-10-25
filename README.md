# caddy-image-optimizer

A [Caddy](https://caddyserver.com) HTTP handler that resizes and re-encodes images on demand and caches the results on disk. It serves JPEG, PNG and WebP files from your site root, picks WebP automatically for browsers that accept it, and leaves everything else to `file_server`.

```
GET /photo.jpg?w=640            -> 640px-wide JPEG
GET /photo.jpg  (Accept: image/webp) -> WebP, same dimensions
GET /photo.jpg?preset=thumb     -> whatever "thumb" is defined as
```

Image work is done by [libvips](https://www.libvips.org) through [govips](https://github.com/davidbyttow/govips). libvips decodes only the pixels it needs (shrink-on-load), so resizing a large JPEG stays cheap.

## Features

- Resize with `w` and `h` (a bounding box, aspect ratio preserved, never upscaled), set quality with `q`, choose the output format with `fmt`.
- Output formats: WebP, JPEG, PNG. Without `fmt`, the format is negotiated from the `Accept` header (WebP if the client accepts it, otherwise the source format).
- Named presets, and a `presets_only` mode that disables raw parameters so the set of variants is fixed by your config.
- Allowlists for widths, heights, qualities and formats, with a configurable response to disallowed values: reject (400), snap to the nearest allowed value, or serve the original.
- Disk cache with a size cap and LRU eviction. Cache entries are keyed on source path, modification time, size and variant, so replacing a source file serves fresh output with no manual purge.
- Identical concurrent requests share one transform. A concurrency cap and queue timeout protect the server, and requests over the cap get a 503 with `Retry-After`.
- Correct HTTP caching: strong `ETag`, `Last-Modified`, conditional requests (304 without touching the cache), `Range`, and `Vary: Accept` wherever the response depends on `Accept`.
- Safe by default: input type is checked by magic bytes, not file extension, dimensions are read from the header before any decode, and limits on source pixels and bytes apply. Metadata (EXIF) is stripped after the orientation is applied.
- Animated GIF, APNG and animated WebP, unsupported types, missing files and requests that need no change all pass through to the next handler unchanged.

## Requirements

- Go 1.26 or newer to build.
- libvips 8.10 or newer with development headers, and a C toolchain. The module uses CGO, so a plain `go build` or `xcaddy build` needs `CGO_ENABLED=1` (the default when a C compiler is present).
  - Debian/Ubuntu: `sudo apt install libvips-dev`
  - Alpine: `apk add vips-dev gcc musl-dev`
  - macOS: `brew install vips`
- libvips must also be installed on the machine that runs the resulting Caddy binary.

## Build

```sh
xcaddy build --with github.com/btafoya/caddy-image-optimizer
```

Check that the module is present:

```sh
./caddy list-modules | grep image_optimizer
# http.handlers.image_optimizer
```

## Quick start

```caddyfile
example.com {
	root * /var/www
	image_optimizer {
		widths 320 640 960 1280
		qualities 40-90
		preset thumb w=320 q=70 fmt=webp
	}
	file_server
}
```

The `image_optimizer` directive is ordered before `file_server` automatically. Inside a `route` block, place it yourself, before `file_server`.

## Request parameters

| Parameter | Meaning |
|---|---|
| `w` | Maximum width in pixels. |
| `h` | Maximum height in pixels. With both `w` and `h` the image is scaled to fit inside the box; aspect ratio is always preserved and images are never enlarged. |
| `q` | Quality, 1 to 100. Defaults to 80 for JPEG and 75 for WebP. Ignored for PNG. |
| `fmt` | `jpg` (or `jpeg`), `png` or `webp`. When absent, the format follows the `Accept` header. |
| `preset` | Name of a configured preset. Explicit `w`, `h`, `q` and `fmt` override the preset's values unless `presets_only` is on. |

Other query parameters are ignored, so cache-busting parameters like `?v=123` are safe. Malformed values (`w=abc`, `q=500`, `fmt=gif`) always return 400. Values that are well-formed but not allowed by your config follow `on_disallowed`.

## Configuration

All options are optional.

```caddyfile
image_optimizer {
	root            {http.vars.root}      # where source images live
	cache_dir       /var/cache/caddy/img  # default: <caddy data dir>/image_optimizer
	cache_max_size  2GiB                  # default: 1GiB

	widths          320 640 960 1280      # allowed widths; default: any up to max_width
	heights         480 720               # allowed heights; default: any up to max_height
	qualities       40-90                 # allowed q range; default: 1-100
	formats         webp jpg png          # allowed output formats; default: all three

	preset thumb    w=320 q=70 fmt=webp
	preset hero     w=1920 q=80
	presets_only                          # optional: disable raw w/h/q/fmt

	on_disallowed   reject                # reject | snap | bypass   (default: reject)
	on_fail         bypass                # bypass | abort           (default: bypass)
	cache_control   "public, max-age=604800"

	limits {
		max_source_pixels  50000000       # default: 50,000,000
		max_source_bytes   50MiB          # default: 50MiB
		max_width          4096           # used when widths is not set
		max_height         4096           # used when heights is not set
		max_concurrent     8              # default: number of CPUs
		queue_timeout      5s             # wait for a free slot, then 503
		transform_timeout  15s
	}
}
```

### Behaviour of the options

- **`on_disallowed`**: what happens when a width, height, quality, format or preset is well-formed but not allowed. `reject` returns 400. `snap` rounds a width or height up to the smallest allowed value at or above the request (or the largest allowed value), clamps quality into range, and falls back to negotiation for a disallowed format. `bypass` serves the original file.
- **`on_fail`**: what happens when a transform itself fails. `bypass` serves the original file, `abort` returns 500.
- **`cache_control`**: set on transformed responses only. If unset, the handler adds no `Cache-Control`; headers from other directives are untouched.
- **`presets_only`**: any raw `w`, `h`, `q` or `fmt` is treated as disallowed and follows `on_disallowed`. Because the variants are fixed by your config, this is the strongest protection against cache flooding.
- **Oversize sources**: a request with explicit parameters for a source above `max_source_pixels` or `max_source_bytes` returns 422. A request that only needs `Accept` negotiation gets the original instead.

### JSON

The module ID is `http.handlers.image_optimizer`:

```json
{
	"handler": "image_optimizer",
	"root": "/var/www",
	"widths": [320, 640, 960],
	"presets": { "thumb": { "w": 320, "q": 70, "fmt": "webp" } },
	"on_disallowed": "snap",
	"limits": { "max_concurrent": 4, "transform_timeout": "15s" }
}
```

## Operational notes

- **Public sites should set `widths` (or use `presets_only`).** Without an allowlist, every distinct `w` creates a separate cache entry. The cache cap and LRU eviction bound disk use, but an attacker can still churn the cache and burn CPU.
- **Caching proxies and CDNs.** Responses that depend on `Accept` carry `Vary: Accept`. Use a CDN that honours it, or request an explicit `fmt` in your markup.
- **Several Caddy instances** can share a cache directory safely (writes are atomic), but each keeps its own size accounting, so give each its own directory if you rely on the size cap.
- **Cache directory** is created with mode `0700`. Entries left over from an interrupted write are cleaned up at startup.
- **libvips cannot cancel a running transform.** If one exceeds `transform_timeout` the request fails, but the transform finishes in the background and keeps its concurrency slot until it does.
- **Reloads.** libvips is initialised once per process and never shut down, so libvips-level settings cannot change on a config reload.

## Limitations

- Local files only. There is no remote fetch and no `reverse_proxy` response processing.
- Scale-to-fit only: no crop, cover or pad modes.
- No AVIF output yet.
- Animated images are served unchanged.
- The cache is local to the machine and is not shared across hosts.

## Development

```sh
go vet ./...
go test -race ./...
```

The tests need libvips installed. The tests cover parameter handling, caching, conditional requests, EXIF orientation, concurrency, limits and the Caddyfile parser. The design rationale is in [DESIGN.md](DESIGN.md) and the original requirements are in [REQUIREMENTS.md](REQUIREMENTS.md).

## License

[MIT](LICENSE)
