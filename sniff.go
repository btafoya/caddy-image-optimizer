package imageoptimizer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"io"
	"os"

	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

var errUnsupported = errors.New("unsupported image type")

type srcInfo struct {
	Fmt      string // jpeg, png, webp
	W, H     int
	Animated bool
}

// probe identifies the source by magic bytes and reads its dimensions from
// the header only (no pixel decode), so size limits are checked before libvips
// sees the file. Anything but JPEG/PNG/WebP is errUnsupported.
func probe(path string) (si srcInfo, err error) {
	f, err := os.Open(path)
	if err != nil {
		return si, err
	}
	defer f.Close()

	var head [32]byte
	n, _ := io.ReadFull(f, head[:])
	h := head[:n]
	switch {
	case bytes.HasPrefix(h, []byte("\xff\xd8\xff")):
		si.Fmt = "jpeg"
	case bytes.HasPrefix(h, []byte("\x89PNG\r\n\x1a\n")):
		si.Fmt = "png"
		si.Animated = pngAnimated(f)
	case n >= 21 && string(h[:4]) == "RIFF" && string(h[8:12]) == "WEBP":
		si.Fmt = "webp"
		si.Animated = string(h[12:16]) == "VP8X" && h[20]&0x02 != 0 // VP8X animation flag
	default:
		return si, errUnsupported
	}
	if si.Animated {
		return si, nil
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return si, err
	}
	cfg, _, err := image.DecodeConfig(f)
	si.W, si.H = cfg.Width, cfg.Height
	return si, err
}

// pngAnimated walks chunks up to the first IDAT looking for APNG's acTL.
func pngAnimated(f *os.File) bool {
	if _, err := f.Seek(8, io.SeekStart); err != nil {
		return false
	}
	var ch [8]byte
	for i := 0; i < 64; i++ { // ponytail: 64 pre-IDAT chunks is far beyond real files
		if _, err := io.ReadFull(f, ch[:]); err != nil {
			return false
		}
		switch string(ch[4:]) {
		case "acTL":
			return true
		case "IDAT", "IEND":
			return false
		}
		if _, err := f.Seek(int64(binary.BigEndian.Uint32(ch[:4]))+4, io.SeekCurrent); err != nil {
			return false
		}
	}
	return false
}
