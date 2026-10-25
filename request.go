package imageoptimizer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// Variant is a fully normalized transform request: it is the cache identity.
type Variant struct {
	W, H, Q int
	Fmt     string
}

var formatInfo = map[string]struct {
	mime, ext string
	defaultQ  int
}{
	"jpeg": {"image/jpeg", "jpg", 80},
	"png":  {"image/png", "png", 0}, // lossless: quality does not apply
	"webp": {"image/webp", "webp", 75},
}

var errBypass = errors.New("bypass to original")

func numCPU() int { return runtime.NumCPU() }

func normFmt(s string) (string, bool) {
	switch strings.ToLower(s) {
	case "jpg", "jpeg":
		return "jpeg", true
	case "png":
		return "png", true
	case "webp":
		return "webp", true
	}
	return s, s == ""
}

func badRequest(format string, a ...any) error {
	return caddyhttp.Error(http.StatusBadRequest, fmt.Errorf(format, a...))
}

// disallow applies on_disallowed to a value outside the allow rules:
// reject -> 400, bypass -> errBypass, snap -> nil (caller snaps the value).
func (m *Middleware) disallow(msg string) error {
	switch m.OnDisallowed {
	case "bypass":
		return errBypass
	case "snap":
		return nil
	}
	return badRequest("%s", msg)
}

// snap rounds up to the smallest allowed value >= n, else the largest allowed.
func snap(n int, sorted []int, max int) int {
	for _, c := range sorted {
		if c >= n {
			return c
		}
	}
	if len(sorted) > 0 {
		return sorted[len(sorted)-1]
	}
	return max
}

func (m *Middleware) dim(k string, n int, list []int, max int) (int, error) {
	ok := n <= max
	if len(list) > 0 {
		ok = slices.Contains(list, n)
	}
	if ok {
		return n, nil
	}
	if err := m.disallow(fmt.Sprintf("%s=%d is not allowed", k, n)); err != nil {
		return 0, err
	}
	return snap(n, list, max), nil
}

func posInt(k, s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, badRequest("invalid %s", k)
	}
	return n, nil
}

// plan turns a query + Accept header into a normalized Variant.
//   - negotiated: the output format depends on Accept (response needs Vary).
//   - explicit: the client asked for a transform via query params.
//   - identity: the variant equals the source, so serve the original.
func (m *Middleware) plan(q url.Values, accept, srcFmt string) (v Variant, negotiated, explicit, identity bool, err error) {
	if name := q.Get("preset"); name != "" {
		explicit = true
		if p, ok := m.Presets[name]; ok {
			v = Variant{W: p.W, H: p.H, Q: p.Q, Fmt: p.Fmt}
		} else if err = m.disallow("unknown preset " + name); err != nil {
			return
		}
	}
	for _, k := range [...]string{"w", "h", "q", "fmt"} {
		s := q.Get(k)
		if s == "" {
			continue
		}
		explicit = true
		if m.PresetsOnly {
			if err = m.disallow("raw " + k + " is not allowed (presets_only)"); err != nil {
				return
			}
			continue
		}
		switch k {
		case "w", "h":
			var n int
			if n, err = posInt(k, s); err != nil {
				return
			}
			if k == "w" {
				v.W, err = m.dim(k, n, m.Widths, m.Limits.MaxWidth)
			} else {
				v.H, err = m.dim(k, n, m.Heights, m.Limits.MaxHeight)
			}
			if err != nil {
				return
			}
		case "q":
			var n int
			if n, err = posInt(k, s); err != nil || n > 100 {
				return v, false, explicit, false, badRequest("invalid q")
			}
			if n < m.QualityMin || n > m.QualityMax {
				if err = m.disallow(fmt.Sprintf("q=%d is not allowed", n)); err != nil {
					return
				}
				n = min(max(n, m.QualityMin), m.QualityMax)
			}
			v.Q = n
		case "fmt":
			f, ok := normFmt(s)
			if !ok {
				return v, false, explicit, false, badRequest("invalid fmt")
			}
			if !slices.Contains(m.Formats, f) {
				if err = m.disallow("fmt=" + f + " is not allowed"); err != nil {
					return
				}
				f = "" // snap: fall back to negotiation
			}
			v.Fmt = f
		}
	}

	negotiated = v.Fmt == "" && srcFmt != "webp" && slices.Contains(m.Formats, "webp")
	if v.Fmt == "" {
		v.Fmt = srcFmt
		if negotiated && acceptsWebP(accept) {
			v.Fmt = "webp"
		}
	}
	qGiven := v.Q != 0 && v.Fmt != "png"
	identity = v.W == 0 && v.H == 0 && !qGiven && v.Fmt == srcFmt
	if v.Fmt == "png" {
		v.Q = 0
	} else if v.Q == 0 {
		v.Q = formatInfo[v.Fmt].defaultQ
	}
	return
}

// acceptsWebP reports whether the Accept header lists image/webp with q > 0.
func acceptsWebP(accept string) bool {
	for _, part := range strings.Split(accept, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil || mt != "image/webp" {
			continue
		}
		if f, err := strconv.ParseFloat(params["q"], 64); params["q"] != "" && (err != nil || f == 0) {
			return false
		}
		return true
	}
	return false
}

// cacheName is the cache identity: source path, source mtime+size, variant.
// Replacing the source changes mtime/size, so the old entry is never hit again.
func cacheName(src string, mtimeNs, size int64, v Variant) (name, etag string) {
	sum := sha256.Sum256([]byte(fmt.Sprintf("v1|%s|%d|%d|%d|%d|%d|%s", src, mtimeNs, size, v.W, v.H, v.Q, v.Fmt)))
	h := hex.EncodeToString(sum[:16])
	return h + "." + formatInfo[v.Fmt].ext, `"` + h + `"`
}

// etagMatches implements If-None-Match for a strong ETag (weak prefix ignored).
func etagMatches(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == etag || t == "*" {
			return true
		}
	}
	return false
}
