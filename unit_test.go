package imageoptimizer

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func TestProbe(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]struct {
		data     []byte
		fmt      string
		animated bool
		err      bool
	}{
		"jpeg":  {data: jpegBytes(t, 30, 20), fmt: "jpeg"},
		"png":   {data: pngBytes(t, 30, 20, 255), fmt: "png"},
		"apng":  {data: apng(pngBytes(t, 30, 20, 255)), fmt: "png", animated: true},
		"awebp": {data: append([]byte("RIFF\x00\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00\x02"), make([]byte, 20)...), fmt: "webp", animated: true},
		"text":  {data: []byte("hello world, definitely not an image"), err: true},
	}
	for name, c := range cases {
		p := filepath.Join(dir, name)
		os.WriteFile(p, c.data, 0o644)
		si, err := probe(p)
		if (err != nil) != c.err {
			t.Errorf("%s: err=%v", name, err)
			continue
		}
		if !c.err && (si.Fmt != c.fmt || si.Animated != c.animated) {
			t.Errorf("%s: got %+v", name, si)
		}
	}
	si, _ := probe(filepath.Join(dir, "jpeg"))
	if si.W != 30 || si.H != 20 {
		t.Errorf("dims %dx%d, want 30x20", si.W, si.H)
	}
}

// apng inserts an acTL chunk right after IHDR.
func apng(png []byte) []byte {
	actl := []byte{0, 0, 0, 8, 'a', 'c', 'T', 'L', 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0}
	return bytes.Join([][]byte{png[:33], actl, png[33:]}, nil)
}

func TestAcceptsWebP(t *testing.T) {
	for accept, want := range map[string]bool{
		"image/webp":                  true,
		"image/avif, image/webp, */*": true,
		"image/webp;q=0.8":            true,
		"image/webp;q=0":              false,
		"image/png":                   false,
		"*/*":                         false,
		"":                            false,
	} {
		if got := acceptsWebP(accept); got != want {
			t.Errorf("%q: got %v want %v", accept, got, want)
		}
	}
}

func TestEtagMatches(t *testing.T) {
	for _, c := range []struct {
		h    string
		want bool
	}{{`"abc"`, true}, {`W/"abc"`, true}, {`"x", "abc"`, true}, {`*`, true}, {`"abd"`, false}, {``, false}} {
		if got := etagMatches(c.h, `"abc"`); got != c.want {
			t.Errorf("%q: got %v", c.h, got)
		}
	}
}

func TestCacheNameStableAndDistinct(t *testing.T) {
	v := Variant{W: 100, Q: 80, Fmt: "jpeg"}
	a, ea := cacheName("/x/a.jpg", 1, 2, v)
	b, _ := cacheName("/x/a.jpg", 1, 2, v)
	if a != b {
		t.Fatal("key not stable")
	}
	for name, other := range map[string]string{
		"mtime":   first(cacheName("/x/a.jpg", 9, 2, v)),
		"size":    first(cacheName("/x/a.jpg", 1, 9, v)),
		"variant": first(cacheName("/x/a.jpg", 1, 2, Variant{W: 101, Q: 80, Fmt: "jpeg"})),
		"path":    first(cacheName("/x/b.jpg", 1, 2, v)),
	} {
		if other == a {
			t.Errorf("%s change did not change key", name)
		}
	}
	if ea == "" || ea[0] != '"' {
		t.Errorf("etag %q", ea)
	}
}

func first(a, _ string) string { return a }

func TestCacheEvictsLeastRecentlyUsedAndRebuilds(t *testing.T) {
	dir := t.TempDir()
	c, err := openCache(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 40)
	for _, n := range []string{"aa1.jpg", "bb2.jpg"} {
		if _, err := c.put(n, blob); err != nil {
			t.Fatal(err)
		}
	}
	c.get("aa1.jpg") // aa1 is now newer than bb2
	c.put("cc3.jpg", blob)
	if _, ok := c.get("bb2.jpg"); ok {
		t.Error("bb2 (least recently used) should be evicted")
	}
	for _, n := range []string{"aa1.jpg", "cc3.jpg"} {
		if _, ok := c.get(n); !ok {
			t.Errorf("%s should survive", n)
		}
	}
	if c.total > 100 {
		t.Errorf("total %d exceeds cap", c.total)
	}

	c2, err := openCache(dir, 100) // restart: index rebuilt from disk
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c2.get("cc3.jpg"); !ok || c2.total != 80 {
		t.Errorf("rebuild: total=%d", c2.total)
	}
}

func TestCacheDropsFilesDeletedBehindItsBack(t *testing.T) {
	c, _ := openCache(t.TempDir(), 1000)
	p, _ := c.put("zz9.png", []byte("data"))
	os.Remove(p)
	if _, ok := c.get("zz9.png"); ok {
		t.Fatal("deleted file reported as hit")
	}
	if c.total != 0 {
		t.Fatalf("total %d after drop", c.total)
	}
}

func TestCaddyfile(t *testing.T) {
	src := `image_optimizer {
		root /srv
		cache_dir /tmp/c
		cache_max_size 2GiB
		widths 320 640
		qualities 40-90
		formats webp jpg
		preset thumb w=320 q=70 fmt=webp
		presets_only
		on_disallowed snap
		on_fail abort
		cache_control "public, max-age=60"
		limits {
			max_source_pixels 1000
			max_concurrent 2
			queue_timeout 3s
		}
	}`
	m := new(Middleware)
	if err := m.UnmarshalCaddyfile(caddyfile.NewTestDispenser(src)); err != nil {
		t.Fatal(err)
	}
	m.defaults()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(m.Root, m.CacheMaxSize, m.Widths, m.QualityMin, m.QualityMax, m.Formats, m.Presets["thumb"], m.PresetsOnly,
		m.OnDisallowed, m.OnFail, m.CacheControl, m.Limits.MaxSourcePixels, m.Limits.MaxConcurrent, int64(m.Limits.QueueTimeout))
	want := fmt.Sprint("/srv", int64(2<<30), []int{320, 640}, 40, 90, []string{"webp", "jpeg"}, Preset{W: 320, Q: 70, Fmt: "webp"}, true,
		"snap", "abort", "public, max-age=60", 1000, 2, int64(3e9))
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

func TestCaddyfileAndValidateRejectBadInput(t *testing.T) {
	for _, src := range []string{
		"image_optimizer {\n bogus 1\n}",
		"image_optimizer {\n widths a b\n}",
		"image_optimizer {\n qualities 90\n}",
		"image_optimizer {\n preset p w=nope\n}",
	} {
		if err := new(Middleware).UnmarshalCaddyfile(caddyfile.NewTestDispenser(src)); err == nil {
			t.Errorf("accepted %q", src)
		}
	}
	for name, edit := range map[string]func(*Middleware){
		"on_disallowed": func(m *Middleware) { m.OnDisallowed = "maybe" },
		"on_fail":       func(m *Middleware) { m.OnFail = "explode" },
		"format":        func(m *Middleware) { m.Formats = []string{"gif"} },
		"quality":       func(m *Middleware) { m.QualityMin, m.QualityMax = 90, 40 },
		"width":         func(m *Middleware) { m.Widths = []int{-1} },
	} {
		m := new(Middleware)
		edit(m)
		m.defaults()
		if m.Validate() == nil {
			t.Errorf("%s: Validate accepted bad config", name)
		}
	}
}
