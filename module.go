// Package imageoptimizer is a Caddy handler that resizes and re-encodes
// JPEG/PNG/WebP files from the site root on demand, with a disk cache.
package imageoptimizer

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

// Middleware is the http.handlers.image_optimizer module.
type Middleware struct {
	Root         string            `json:"root,omitempty"`
	CacheDir     string            `json:"cache_dir,omitempty"`
	CacheMaxSize int64             `json:"cache_max_size,omitempty"`
	Widths       []int             `json:"widths,omitempty"`
	Heights      []int             `json:"heights,omitempty"`
	QualityMin   int               `json:"quality_min,omitempty"`
	QualityMax   int               `json:"quality_max,omitempty"`
	Formats      []string          `json:"formats,omitempty"`
	Presets      map[string]Preset `json:"presets,omitempty"`
	PresetsOnly  bool              `json:"presets_only,omitempty"`
	OnDisallowed string            `json:"on_disallowed,omitempty"` // reject | snap | bypass
	OnFail       string            `json:"on_fail,omitempty"`       // bypass | abort
	CacheControl string            `json:"cache_control,omitempty"`
	Limits       Limits            `json:"limits,omitempty"`

	logger    *zap.Logger
	cache     *diskCache
	sem       chan struct{}
	flight    singleflight.Group
	transform func(src string, v Variant) ([]byte, error) // stubbed in tests
}

// CaddyModule implements caddy.Module.
func (*Middleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.image_optimizer",
		New: func() caddy.Module { return new(Middleware) },
	}
}

// Provision implements caddy.Provisioner.
func (m *Middleware) Provision(ctx caddy.Context) error { return m.setup(ctx.Logger()) }

func (m *Middleware) setup(logger *zap.Logger) error {
	m.logger = logger
	m.defaults()
	startVips()
	c, err := openCache(m.CacheDir, m.CacheMaxSize)
	if err != nil {
		return err
	}
	m.cache = c
	m.sem = make(chan struct{}, m.Limits.MaxConcurrent)
	if m.transform == nil {
		m.transform = transform
	}
	return nil
}

func isImageExt(p string) bool {
	i := strings.LastIndexByte(p, '.')
	if i < 0 {
		return false
	}
	switch strings.ToLower(p[i:]) {
	case ".jpg", ".jpeg", ".png", ".webp":
		return true
	}
	return false
}

// ServeHTTP implements caddyhttp.MiddlewareHandler. Anything it does not
// transform goes to next (file_server), which also owns every 404.
func (m *Middleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return next.ServeHTTP(w, r)
	}
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	src := caddyhttp.SanitizedPathJoin(repl.ReplaceAll(m.Root, "."), r.URL.Path)
	if !isImageExt(src) {
		return next.ServeHTTP(w, r)
	}
	info, err := os.Stat(src)
	if err != nil || !info.Mode().IsRegular() {
		return next.ServeHTTP(w, r)
	}
	si, err := probe(src)
	if err != nil || si.Animated {
		return next.ServeHTTP(w, r)
	}

	v, negotiated, explicit, identity, err := m.plan(r.URL.Query(), r.Header.Get("Accept"), si.Fmt)
	if negotiated {
		w.Header().Add("Vary", "Accept") // set on every path, passthrough included
	}
	if errors.Is(err, errBypass) {
		return next.ServeHTTP(w, r)
	}
	if err != nil {
		return err
	}
	if identity {
		return next.ServeHTTP(w, r)
	}
	if info.Size() > m.Limits.MaxSourceBytes || int64(si.W)*int64(si.H) > m.Limits.MaxSourcePixels {
		if explicit {
			return caddyhttp.Error(http.StatusUnprocessableEntity, errors.New("source image exceeds size limits"))
		}
		return next.ServeHTTP(w, r)
	}

	name, etag := cacheName(src, info.ModTime().UnixNano(), info.Size(), v)
	h := w.Header()
	h.Set("ETag", etag)
	if m.CacheControl != "" {
		h.Set("Cache-Control", m.CacheControl)
	}
	// Answer conditional requests without touching the cache or libvips.
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}

	p, err := m.build(r.Context(), name, src, v)
	switch {
	case err == nil:
	case r.Context().Err() != nil:
		return nil // client went away
	case errors.Is(err, errBusy):
		h.Set("Retry-After", "1")
		return caddyhttp.Error(http.StatusServiceUnavailable, err)
	default:
		m.logger.Warn("transform failed", zap.String("path", src), zap.Error(err))
		h.Del("ETag")
		if m.OnFail == "abort" {
			return caddyhttp.Error(http.StatusInternalServerError, err)
		}
		return next.ServeHTTP(w, r)
	}

	f, err := os.Open(p)
	if err != nil {
		return caddyhttp.Error(http.StatusInternalServerError, err)
	}
	defer f.Close()
	h.Set("Content-Type", formatInfo[v.Fmt].mime)
	http.ServeContent(w, r, "", info.ModTime(), f) // Range, If-Modified-Since, HEAD
	return nil
}

var (
	errBusy    = errors.New("image optimizer busy")
	errTimeout = errors.New("transform timed out")
)

// build returns the cached file for name, transforming src on a miss.
// Identical concurrent misses share one transform.
func (m *Middleware) build(ctx context.Context, name, src string, v Variant) (string, error) {
	if p, ok := m.cache.get(name); ok {
		return p, nil
	}
	ch := m.flight.DoChan(name, func() (any, error) {
		if p, ok := m.cache.get(name); ok {
			return p, nil
		}
		queue := time.NewTimer(time.Duration(m.Limits.QueueTimeout))
		defer queue.Stop()
		select {
		case m.sem <- struct{}{}:
		case <-queue.C:
			return nil, errBusy
		}
		type result struct {
			path string
			err  error
		}
		done := make(chan result, 1)
		// ponytail: libvips cannot be cancelled. On timeout the goroutine runs to
		// completion (still holding its slot, still filling the cache) while the
		// requests fail; add vips-side cancellation only if timeouts become common.
		go func() {
			defer func() { <-m.sem }()
			b, err := m.transform(src, v)
			if err != nil {
				done <- result{err: err}
				return
			}
			p, err := m.cache.put(name, b)
			done <- result{p, err}
		}()
		select {
		case r := <-done:
			return r.path, r.err
		case <-time.After(time.Duration(m.Limits.TransformTimeout)):
			return nil, errTimeout
		}
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return "", r.Err
		}
		return r.Val.(string), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

var (
	_ caddy.Provisioner           = (*Middleware)(nil)
	_ caddyhttp.MiddlewareHandler = (*Middleware)(nil)
)
