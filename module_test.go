package imageoptimizer

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResizeCachesAndAnswers304(t *testing.T) {
	m, root := newTestMiddleware(t, nil)
	n := countTransforms(m)
	writeFile(t, root, "a.jpg", jpegBytes(t, 800, 600))

	rec, next, err := serve(t, m, "/a.jpg?w=400", nil)
	if err != nil || next || rec.Code != 200 {
		t.Fatalf("code=%d next=%v err=%v", rec.Code, next, err)
	}
	if w, h := dims(t, rec.Body.Bytes()); w != 400 || h != 300 {
		t.Fatalf("got %dx%d, want 400x300", w, h)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("content-type %q", ct)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}

	serve(t, m, "/a.jpg?w=400", nil) // hit
	rec, _, _ = serve(t, m, "/a.jpg?w=400", map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("conditional: code %d", rec.Code)
	}
	if n.Load() != 1 {
		t.Fatalf("transforms=%d, want 1", n.Load())
	}
}

func TestNeverUpscales(t *testing.T) {
	m, root := newTestMiddleware(t, nil)
	writeFile(t, root, "a.jpg", jpegBytes(t, 200, 100))
	rec, _, err := serve(t, m, "/a.jpg?w=1000", nil)
	if err != nil {
		t.Fatal(err)
	}
	if w, _ := dims(t, rec.Body.Bytes()); w != 200 {
		t.Fatalf("width %d, want 200", w)
	}
}

func TestAcceptNegotiation(t *testing.T) {
	m, root := newTestMiddleware(t, nil)
	writeFile(t, root, "a.png", pngBytes(t, 100, 100, 255))

	rec, next, err := serve(t, m, "/a.png", map[string]string{"Accept": "image/avif,image/webp,*/*"})
	if err != nil || next {
		t.Fatalf("next=%v err=%v", next, err)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/webp" {
		t.Fatalf("content-type %q", ct)
	}
	if rec.Header().Get("Vary") != "Accept" {
		t.Fatalf("Vary=%q", rec.Header().Get("Vary"))
	}

	rec, next, _ = serve(t, m, "/a.png", map[string]string{"Accept": "image/png"})
	if !next {
		t.Fatal("no webp in Accept: want passthrough")
	}
	if rec.Header().Get("Vary") != "Accept" {
		t.Fatal("passthrough must still carry Vary: Accept")
	}

	_, next, _ = serve(t, m, "/a.png", map[string]string{"Accept": "image/webp;q=0"})
	if !next {
		t.Fatal("webp;q=0 must not negotiate webp")
	}
}

func TestAlphaPNGToJPEGFlattens(t *testing.T) {
	m, root := newTestMiddleware(t, nil)
	writeFile(t, root, "a.png", pngBytes(t, 64, 64, 100))
	rec, _, err := serve(t, m, "/a.png?fmt=jpg", nil)
	if err != nil || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("err=%v ct=%q", err, rec.Header().Get("Content-Type"))
	}
}

func TestEXIFOrientationApplied(t *testing.T) {
	m, root := newTestMiddleware(t, nil)
	// 200x100 stored, orientation 6 (rotate 90): displays as 100x200.
	writeFile(t, root, "r.jpg", withOrientation(jpegBytes(t, 200, 100), 6))
	rec, _, err := serve(t, m, "/r.jpg?w=50", nil)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := dims(t, rec.Body.Bytes()); w != 50 || h != 100 {
		t.Fatalf("got %dx%d, want 50x100 (autorotated)", w, h)
	}
}

func TestOnDisallowed(t *testing.T) {
	cases := []struct {
		mode              string
		wantNext          bool
		wantStatus, wantW int
	}{
		{"reject", false, 400, 0},
		{"snap", false, 0, 640},
		{"bypass", true, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			m, root := newTestMiddleware(t, func(m *Middleware) {
				m.Widths = []int{320, 640}
				m.OnDisallowed = c.mode
			})
			writeFile(t, root, "a.jpg", jpegBytes(t, 1000, 500))
			rec, next, err := serve(t, m, "/a.jpg?w=500", nil)
			if next != c.wantNext || statusOf(err) != c.wantStatus {
				t.Fatalf("next=%v status=%d err=%v", next, statusOf(err), err)
			}
			if c.wantW != 0 {
				if w, _ := dims(t, rec.Body.Bytes()); w != c.wantW {
					t.Fatalf("snapped to %d, want %d", w, c.wantW)
				}
			}
		})
	}
}

func TestSnapAboveLargestAllowedUsesLargest(t *testing.T) {
	m, root := newTestMiddleware(t, func(m *Middleware) {
		m.Widths = []int{100, 200}
		m.OnDisallowed = "snap"
	})
	writeFile(t, root, "a.jpg", jpegBytes(t, 1000, 500))
	rec, _, err := serve(t, m, "/a.jpg?w=999", nil)
	if err != nil {
		t.Fatal(err)
	}
	if w, _ := dims(t, rec.Body.Bytes()); w != 200 {
		t.Fatalf("width %d, want 200", w)
	}
}

func TestMalformedParamsAre400(t *testing.T) {
	m, root := newTestMiddleware(t, func(m *Middleware) { m.OnDisallowed = "bypass" })
	writeFile(t, root, "a.jpg", jpegBytes(t, 100, 100))
	for _, q := range []string{"w=abc", "w=-5", "w=0", "q=101", "q=x", "fmt=gif"} {
		if _, _, err := serve(t, m, "/a.jpg?"+q, nil); statusOf(err) != 400 {
			t.Errorf("%s: status %d err=%v, want 400 even with on_disallowed=bypass", q, statusOf(err), err)
		}
	}
}

func TestPresetsOnly(t *testing.T) {
	m, root := newTestMiddleware(t, func(m *Middleware) {
		m.PresetsOnly = true
		m.Presets = map[string]Preset{"thumb": {W: 50, Q: 60, Fmt: "webp"}}
	})
	writeFile(t, root, "a.jpg", jpegBytes(t, 400, 400))

	rec, _, err := serve(t, m, "/a.jpg?preset=thumb", nil)
	if err != nil || rec.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("preset: err=%v ct=%q", err, rec.Header().Get("Content-Type"))
	}
	if w, _ := dims(t, rec.Body.Bytes()); w != 50 {
		t.Fatalf("preset width %d", w)
	}
	if _, _, err = serve(t, m, "/a.jpg?w=300", nil); statusOf(err) != 400 {
		t.Fatalf("raw w under presets_only: status %d", statusOf(err))
	}
	if _, _, err = serve(t, m, "/a.jpg?preset=nope", nil); statusOf(err) != 400 {
		t.Fatalf("unknown preset: status %d", statusOf(err))
	}
}

func TestSourceLimits(t *testing.T) {
	m, root := newTestMiddleware(t, func(m *Middleware) { m.Limits.MaxSourcePixels = 1000 })
	writeFile(t, root, "big.jpg", jpegBytes(t, 100, 100))
	if _, _, err := serve(t, m, "/big.jpg?w=10", nil); statusOf(err) != 422 {
		t.Fatalf("explicit transform of oversize source: status %d err=%v", statusOf(err), err)
	}
	_, next, err := serve(t, m, "/big.jpg", map[string]string{"Accept": "image/webp"})
	if err != nil || !next {
		t.Fatalf("negotiation-only on oversize source: next=%v err=%v, want passthrough", next, err)
	}
}

func TestPassthrough(t *testing.T) {
	m, root := newTestMiddleware(t, nil)
	writeFile(t, root, "a.jpg", jpegBytes(t, 50, 50))
	writeFile(t, root, "fake.jpg", []byte("not an image at all, just text"))
	writeFile(t, root, "notes.txt", []byte("hi"))
	for _, target := range []string{
		"/a.jpg",                     // identity: nothing requested
		"/missing.jpg?w=10",          // file_server owns the 404
		"/fake.jpg?w=10",             // wrong magic bytes
		"/notes.txt?w=10",            // not an image extension
		"/../../etc/passwd.png?w=10", // traversal is sanitized into root
	} {
		if _, next, err := serve(t, m, target, nil); !next || err != nil {
			t.Errorf("%s: next=%v err=%v, want passthrough", target, next, err)
		}
	}
}

func TestSourceChangeInvalidatesCache(t *testing.T) {
	m, root := newTestMiddleware(t, nil)
	n := countTransforms(m)
	writeFile(t, root, "a.jpg", jpegBytes(t, 400, 400))
	serve(t, m, "/a.jpg?w=100", nil)
	time.Sleep(10 * time.Millisecond)
	writeFile(t, root, "a.jpg", jpegBytes(t, 500, 500))
	serve(t, m, "/a.jpg?w=100", nil)
	if n.Load() != 2 {
		t.Fatalf("transforms=%d, want 2 after source change", n.Load())
	}
}

func TestIdenticalMissesShareOneTransform(t *testing.T) {
	m, root := newTestMiddleware(t, nil)
	writeFile(t, root, "a.jpg", jpegBytes(t, 400, 400))
	var calls int32
	var mu sync.Mutex
	real := m.transform
	m.transform = func(src string, v Variant) ([]byte, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(150 * time.Millisecond) // let the others pile up
		return real(src, v)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec, _, err := serve(t, m, "/a.jpg?w=100", nil); err != nil || rec.Code != 200 {
				t.Errorf("code=%d err=%v", rec.Code, err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("transforms=%d, want 1", calls)
	}
}

func TestBusyReturns503(t *testing.T) {
	m, root := newTestMiddleware(t, func(m *Middleware) {
		m.Limits.MaxConcurrent = 1
		m.Limits.QueueTimeout = 50e6 // 50ms
	})
	writeFile(t, root, "a.jpg", jpegBytes(t, 400, 400))
	release := make(chan struct{})
	m.transform = func(string, Variant) ([]byte, error) { <-release; return []byte("x"), nil }
	go serve(t, m, "/a.jpg?w=100", nil) // occupies the only slot
	time.Sleep(50 * time.Millisecond)
	rec, _, err := serve(t, m, "/a.jpg?w=200", nil)
	close(release)
	if statusOf(err) != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d retry-after=%q err=%v", statusOf(err), rec.Header().Get("Retry-After"), err)
	}
}

func TestOnFail(t *testing.T) {
	for _, mode := range []string{"bypass", "abort"} {
		t.Run(mode, func(t *testing.T) {
			m, root := newTestMiddleware(t, func(m *Middleware) { m.OnFail = mode })
			writeFile(t, root, "a.jpg", jpegBytes(t, 100, 100))
			m.transform = func(string, Variant) ([]byte, error) { return nil, errTimeout }
			rec, next, err := serve(t, m, "/a.jpg?w=50", nil)
			if mode == "bypass" && (!next || err != nil) {
				t.Fatalf("next=%v err=%v", next, err)
			}
			if mode == "abort" && statusOf(err) != 500 {
				t.Fatalf("status %d", statusOf(err))
			}
			if rec.Header().Get("ETag") != "" {
				t.Fatal("failed transform must not leak a variant ETag")
			}
		})
	}
}

func TestCacheControlOverrideAndHEAD(t *testing.T) {
	m, root := newTestMiddleware(t, func(m *Middleware) { m.CacheControl = "public, max-age=60" })
	writeFile(t, root, "a.jpg", jpegBytes(t, 200, 200))
	rec, _, err := serve(t, m, "/a.jpg?w=50", nil)
	if err != nil || !strings.Contains(rec.Header().Get("Cache-Control"), "max-age=60") {
		t.Fatalf("err=%v cc=%q", err, rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get("Last-Modified") == "" {
		t.Fatal("missing Last-Modified")
	}
}
