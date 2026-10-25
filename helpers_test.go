package imageoptimizer

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"golang.org/x/image/webp"
)

func gradient(w, h int, alpha uint8) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{uint8(x * 255 / w), uint8(y * 255 / h), 128, alpha})
		}
	}
	return img
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, gradient(w, h, 255), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func pngBytes(t *testing.T, w, h int, alpha uint8) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, gradient(w, h, alpha)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// withOrientation inserts an EXIF APP1 segment (orientation tag) after SOI.
func withOrientation(jpg []byte, orientation byte) []byte {
	tiff := []byte{'I', 'I', 0x2a, 0, 8, 0, 0, 0, 1, 0, 0x12, 0x01, 3, 0, 1, 0, 0, 0, orientation, 0, 0, 0, 0, 0, 0, 0}
	seg := append([]byte("Exif\x00\x00"), tiff...)
	app1 := append([]byte{0xff, 0xe1, 0, 0}, seg...)
	binary.BigEndian.PutUint16(app1[2:4], uint16(len(seg)+2))
	return append(append(append([]byte{}, jpg[:2]...), app1...), jpg[2:]...)
}

func writeFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// newTestMiddleware returns a provisioned middleware rooted at a temp dir.
func newTestMiddleware(t *testing.T, edit func(*Middleware)) (*Middleware, string) {
	t.Helper()
	root := t.TempDir()
	m := &Middleware{Root: root, CacheDir: filepath.Join(t.TempDir(), "cache")}
	if edit != nil {
		edit(m)
	}
	if err := m.setup(zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	return m, root
}

// serve runs one request through the middleware. nextHit reports passthrough.
func serve(t *testing.T, m *Middleware, target string, hdr map[string]string) (rec *httptest.ResponseRecorder, nextHit bool, err error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req = req.WithContext(context.WithValue(req.Context(), caddy.ReplacerCtxKey, caddy.NewReplacer()))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec = httptest.NewRecorder()
	err = m.ServeHTTP(rec, req, caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		nextHit = true
		return nil
	}))
	return
}

func statusOf(err error) int {
	if he, ok := err.(caddyhttp.HandlerError); ok {
		return he.StatusCode
	}
	return 0
}

func dims(t *testing.T, body []byte) (w, h int) {
	t.Helper()
	if len(body) > 12 && string(body[8:12]) == "WEBP" {
		cfg, err := webp.DecodeConfig(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Width, cfg.Height
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height
}

// countTransforms wraps the real transform with a call counter.
func countTransforms(m *Middleware) *atomic.Int32 {
	var n atomic.Int32
	real := m.transform
	m.transform = func(src string, v Variant) ([]byte, error) {
		n.Add(1)
		return real(src, v)
	}
	return &n
}
