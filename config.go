package imageoptimizer

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/dustin/go-humanize"
)

func init() {
	caddy.RegisterModule(&Middleware{})
	httpcaddyfile.RegisterHandlerDirective("image_optimizer", parseCaddyfile)
	httpcaddyfile.RegisterDirectiveOrder("image_optimizer", httpcaddyfile.Before, "file_server")
}

// Preset is a named, admin-defined variant. Zero fields are unset.
type Preset struct {
	W   int    `json:"w,omitempty"`
	H   int    `json:"h,omitempty"`
	Q   int    `json:"q,omitempty"`
	Fmt string `json:"fmt,omitempty"`
}

// Limits bounds the work a request can cause.
type Limits struct {
	MaxSourcePixels  int64          `json:"max_source_pixels,omitempty"`
	MaxSourceBytes   int64          `json:"max_source_bytes,omitempty"`
	MaxWidth         int            `json:"max_width,omitempty"`
	MaxHeight        int            `json:"max_height,omitempty"`
	MaxConcurrent    int            `json:"max_concurrent,omitempty"`
	QueueTimeout     caddy.Duration `json:"queue_timeout,omitempty"`
	TransformTimeout caddy.Duration `json:"transform_timeout,omitempty"`
}

var validFormats = []string{"jpeg", "png", "webp"}

// defaults fills unset fields and normalizes format names. Idempotent.
func (m *Middleware) defaults() {
	if m.Root == "" {
		m.Root = "{http.vars.root}"
	}
	if m.CacheDir == "" {
		m.CacheDir = filepath.Join(caddy.AppDataDir(), "image_optimizer")
	}
	if m.CacheMaxSize == 0 {
		m.CacheMaxSize = 1 << 30
	}
	if m.QualityMin == 0 {
		m.QualityMin = 1
	}
	if m.QualityMax == 0 {
		m.QualityMax = 100
	}
	if len(m.Formats) == 0 {
		m.Formats = []string{"webp", "jpeg", "png"}
	}
	for i, f := range m.Formats {
		m.Formats[i], _ = normFmt(f)
	}
	for k, p := range m.Presets {
		p.Fmt, _ = normFmt(p.Fmt)
		m.Presets[k] = p
	}
	if m.OnDisallowed == "" {
		m.OnDisallowed = "reject"
	}
	if m.OnFail == "" {
		m.OnFail = "bypass"
	}
	l := &m.Limits
	if l.MaxSourcePixels == 0 {
		l.MaxSourcePixels = 50_000_000
	}
	if l.MaxSourceBytes == 0 {
		l.MaxSourceBytes = 50 << 20
	}
	if l.MaxWidth == 0 {
		l.MaxWidth = 4096
	}
	if l.MaxHeight == 0 {
		l.MaxHeight = 4096
	}
	if l.MaxConcurrent == 0 {
		l.MaxConcurrent = numCPU()
	}
	if l.QueueTimeout == 0 {
		l.QueueTimeout = caddy.Duration(5e9)
	}
	if l.TransformTimeout == 0 {
		l.TransformTimeout = caddy.Duration(15e9)
	}
	sort.Ints(m.Widths)
	sort.Ints(m.Heights)
}

// Validate implements caddy.Validator.
func (m *Middleware) Validate() error {
	if !slices.Contains([]string{"reject", "snap", "bypass"}, m.OnDisallowed) {
		return fmt.Errorf("on_disallowed: %q is not reject, snap or bypass", m.OnDisallowed)
	}
	if !slices.Contains([]string{"bypass", "abort"}, m.OnFail) {
		return fmt.Errorf("on_fail: %q is not bypass or abort", m.OnFail)
	}
	for _, f := range m.Formats {
		if !slices.Contains(validFormats, f) {
			return fmt.Errorf("formats: unsupported format %q", f)
		}
	}
	if m.QualityMin < 1 || m.QualityMax > 100 || m.QualityMin > m.QualityMax {
		return fmt.Errorf("qualities: need 1 <= min <= max <= 100, got %d-%d", m.QualityMin, m.QualityMax)
	}
	for _, n := range append(slices.Clone(m.Widths), m.Heights...) {
		if n < 1 {
			return fmt.Errorf("widths/heights: %d is not positive", n)
		}
	}
	for name, p := range m.Presets {
		if p.Fmt != "" && !slices.Contains(validFormats, p.Fmt) {
			return fmt.Errorf("preset %s: unsupported fmt %q", name, p.Fmt)
		}
	}
	l := m.Limits
	if l.MaxSourcePixels < 1 || l.MaxSourceBytes < 1 || l.MaxWidth < 1 || l.MaxHeight < 1 || l.MaxConcurrent < 1 {
		return fmt.Errorf("limits must be positive")
	}
	return nil
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	m := new(Middleware)
	return m, m.UnmarshalCaddyfile(h.Dispenser)
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (m *Middleware) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // directive name
	if d.NextArg() {
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		key := d.Val()
		switch key {
		case "root":
			if !d.AllArgs(&m.Root) {
				return d.ArgErr()
			}
		case "cache_dir":
			if !d.AllArgs(&m.CacheDir) {
				return d.ArgErr()
			}
		case "cache_max_size":
			var s string
			if !d.AllArgs(&s) {
				return d.ArgErr()
			}
			n, err := humanize.ParseBytes(s)
			if err != nil {
				return d.Errf("cache_max_size: %v", err)
			}
			m.CacheMaxSize = int64(n)
		case "widths":
			var err error
			if m.Widths, err = ints(d, key); err != nil {
				return err
			}
		case "heights":
			var err error
			if m.Heights, err = ints(d, key); err != nil {
				return err
			}
		case "qualities":
			var s string
			if !d.AllArgs(&s) {
				return d.ArgErr()
			}
			lo, hi, ok := strings.Cut(s, "-")
			var err1, err2 error
			m.QualityMin, err1 = strconv.Atoi(lo)
			m.QualityMax, err2 = strconv.Atoi(hi)
			if !ok || err1 != nil || err2 != nil {
				return d.Errf("qualities: want MIN-MAX, got %q", s)
			}
		case "formats":
			m.Formats = d.RemainingArgs()
			if len(m.Formats) == 0 {
				return d.ArgErr()
			}
		case "preset":
			args := d.RemainingArgs()
			if len(args) < 2 {
				return d.ArgErr()
			}
			var p Preset
			for _, kv := range args[1:] {
				k, v, _ := strings.Cut(kv, "=")
				n, err := strconv.Atoi(v)
				switch k {
				case "w":
					p.W = n
				case "h":
					p.H = n
				case "q":
					p.Q = n
				case "fmt":
					p.Fmt, err = v, nil
				default:
					err = fmt.Errorf("unknown key")
				}
				if err != nil {
					return d.Errf("preset %s: bad %q", args[0], kv)
				}
			}
			if m.Presets == nil {
				m.Presets = map[string]Preset{}
			}
			m.Presets[args[0]] = p
		case "presets_only":
			m.PresetsOnly = true
			if d.NextArg() {
				b, err := strconv.ParseBool(d.Val())
				if err != nil {
					return d.Errf("presets_only: %v", err)
				}
				m.PresetsOnly = b
			}
		case "on_disallowed":
			if !d.AllArgs(&m.OnDisallowed) {
				return d.ArgErr()
			}
		case "on_fail":
			if !d.AllArgs(&m.OnFail) {
				return d.ArgErr()
			}
		case "cache_control":
			if !d.AllArgs(&m.CacheControl) {
				return d.ArgErr()
			}
		case "limits":
			if err := m.Limits.unmarshal(d); err != nil {
				return err
			}
		default:
			return d.Errf("unknown option %q", key)
		}
	}
	return nil
}

func (l *Limits) unmarshal(d *caddyfile.Dispenser) error {
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		key := d.Val()
		var s string
		if !d.AllArgs(&s) {
			return d.ArgErr()
		}
		var err error
		switch key {
		case "max_source_pixels":
			l.MaxSourcePixels, err = strconv.ParseInt(s, 10, 64)
		case "max_source_bytes":
			var n uint64
			n, err = humanize.ParseBytes(s)
			l.MaxSourceBytes = int64(n)
		case "max_width":
			l.MaxWidth, err = strconv.Atoi(s)
		case "max_height":
			l.MaxHeight, err = strconv.Atoi(s)
		case "max_concurrent":
			l.MaxConcurrent, err = strconv.Atoi(s)
		case "queue_timeout":
			var v caddy.Duration
			v, err = parseDuration(s)
			l.QueueTimeout = v
		case "transform_timeout":
			var v caddy.Duration
			v, err = parseDuration(s)
			l.TransformTimeout = v
		default:
			return d.Errf("unknown limit %q", key)
		}
		if err != nil {
			return d.Errf("%s: %v", key, err)
		}
	}
	return nil
}

func parseDuration(s string) (caddy.Duration, error) {
	v, err := caddy.ParseDuration(s)
	return caddy.Duration(v), err
}

func ints(d *caddyfile.Dispenser, key string) ([]int, error) {
	args := d.RemainingArgs()
	if len(args) == 0 {
		return nil, d.ArgErr()
	}
	out := make([]int, len(args))
	for i, a := range args {
		n, err := strconv.Atoi(a)
		if err != nil {
			return nil, d.Errf("%s: %q is not a number", key, a)
		}
		out[i] = n
	}
	return out, nil
}

var (
	_ caddy.Validator       = (*Middleware)(nil)
	_ caddyfile.Unmarshaler = (*Middleware)(nil)
)
