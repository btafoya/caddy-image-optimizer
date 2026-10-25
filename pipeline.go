package imageoptimizer

import (
	"fmt"
	"sync"

	"github.com/davidbyttow/govips/v2/vips"
)

var vipsOnce sync.Once

// startVips initializes libvips once per process and never shuts it down:
// govips cannot restart, and Caddy re-provisions modules on every reload.
func startVips() {
	vipsOnce.Do(func() {
		vips.LoggingSettings(nil, vips.LogLevelError)
		// nil config: one libvips thread per transform; parallelism comes
		// from the max_concurrent semaphore.
		vips.Startup(nil)
	})
}

// maxCoord is libvips' VIPS_MAX_COORD: "no limit" for the thumbnail box.
const maxCoord = 10_000_000

// transform resizes src into the (w,h) bounding box (never upscaling,
// preserving aspect ratio), applies EXIF orientation, strips metadata and
// encodes as v.Fmt.
func transform(src string, v Variant) ([]byte, error) {
	w, h := v.W, v.H
	if w == 0 {
		w = maxCoord
	}
	if h == 0 {
		h = maxCoord
	}
	// Thumbnail loads from disk with shrink-on-load and autorotates.
	img, err := vips.NewThumbnailWithSizeFromFile(src, w, h, vips.InterestingNone, vips.SizeDown)
	if err != nil {
		return nil, err
	}
	defer img.Close()

	var out []byte
	switch v.Fmt {
	case "jpeg":
		if img.HasAlpha() {
			if err = img.Flatten(&vips.Color{R: 255, G: 255, B: 255}); err != nil {
				return nil, err
			}
		}
		p := vips.NewJpegExportParams()
		p.Quality, p.StripMetadata = v.Q, true
		out, _, err = img.ExportJpeg(p)
	case "png":
		p := vips.NewPngExportParams()
		p.StripMetadata = true
		out, _, err = img.ExportPng(p)
	case "webp":
		p := vips.NewWebpExportParams()
		p.Quality, p.StripMetadata = v.Q, true
		out, _, err = img.ExportWebp(p)
	default:
		err = fmt.Errorf("unsupported output format %q", v.Fmt)
	}
	return out, err
}
