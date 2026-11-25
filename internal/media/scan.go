// Package media validates the draft v1 on-media format. Callers supply exactly
// one logical tape file, returning EOF only at its physical filemark.
package media

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/maxya/TapePilot/internal/tape"
)

var ErrFormat = errors.New("invalid or unsupported tape format")

// Digest covers the complete tape file, including the final record padding.
type Digest struct {
	Bytes  int64
	SHA256 string
}

type counter struct {
	io.Reader
	n       int64
	capture bool
	header  []byte
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.n += int64(n)
	if c.capture {
		if len(c.header)+n > (1<<20)+2048 {
			return n, ErrFormat
		}
		c.header = append(c.header, p[:n]...)
	}
	return n, err
}

// Scan stages validated headers through visit. Its caller must discard all
// staged rows if this scan or the subsequent commit verification fails.
func Scan(r io.Reader, visit func(*tar.Header) error) (Digest, error) {
	return scan(r, func(h *tar.Header, _ io.Reader) error {
		if visit != nil {
			return visit(h)
		}
		return nil
	}, false)
}
func scan(r io.Reader, visit func(*tar.Header, io.Reader) error, metadata bool) (Digest, error) {
	hash := sha256.New()
	raw := counter{Reader: io.TeeReader(r, hash)}
	tr := tar.NewReader(&raw)
	names := map[string]bool{}
	var nameBytes, padding int64
	for {
		before := raw.n
		raw.header = raw.header[:0]
		raw.capture = true
		h, err := tr.Next()
		raw.capture = false
		if err == io.EOF {
			if len(names) == 0 || raw.n-before != padding+1024 {
				return Digest{}, fmt.Errorf("%w: missing terminators", ErrFormat)
			}
			break
		}
		if err != nil {
			return Digest{}, fmt.Errorf("%w: %w", ErrFormat, err)
		}
		if h.Format != tar.FormatUSTAR && h.Format != tar.FormatPAX && !gnuUnicodePAX(raw.header[padding:], h) {
			return Digest{}, fmt.Errorf("%w: TAR variant", ErrFormat)
		}
		if metadata && (h.Format != tar.FormatUSTAR || h.Typeflag != tar.TypeReg) {
			return Digest{}, ErrFormat
		}
		for key := range h.PAXRecords {
			switch key {
			case "path", "linkpath", "size", "mtime", "atime", "ctime", "uid", "gid", "uname", "gname":
			default:
				return Digest{}, fmt.Errorf("%w: PAX keyword", ErrFormat)
			}
		}
		name := h.Name
		switch h.Typeflag {
		case tar.TypeDir:
			name = strings.TrimSuffix(name, "/")
		case tar.TypeReg, tar.TypeLink, tar.TypeSymlink:
		default:
			return Digest{}, fmt.Errorf("%w: member type", ErrFormat)
		}
		if h.Typeflag != tar.TypeReg && h.Size != 0 {
			return Digest{}, ErrFormat
		}
		if !fs.ValidPath(name) || name == "." || strings.ContainsRune(name, 0) || len(name) > 4096 || !utf8.ValidString(h.Linkname) || len(h.Linkname) > 4096 || names[name] {
			return Digest{}, fmt.Errorf("%w: member name", ErrFormat)
		}
		nameBytes += int64(len(name))
		if nameBytes > 128<<20 || len(names) >= 1_000_000 {
			return Digest{}, fmt.Errorf("%w: member budget", ErrFormat)
		}
		names[name] = true
		if err := visit(h, tr); err != nil {
			return Digest{}, err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return Digest{}, err
		}
		padding = (512 - h.Size%512) % 512
	}
	tailSize := (tape.RecordBytes - raw.n%tape.RecordBytes) % tape.RecordBytes
	tail, err := io.ReadAll(io.LimitReader(&raw, tailSize+1))
	if err != nil {
		return Digest{}, err
	}
	if int64(len(tail)) != tailSize || len(bytes.Trim(tail, "\x00")) != 0 {
		return Digest{}, fmt.Errorf("%w: final record padding", ErrFormat)
	}
	return Digest{raw.n, hex.EncodeToString(hash.Sum(nil))}, nil
}

// GNU tar places UTF-8 in the legacy filename field as well as its PAX path
// override. Go reports FormatUnknown for these checksummed USTAR blocks. Admit
// only the exact extended-header + data-header sequence and the UTF-8 fields
// observed in GNU fixtures. archive/tar has already checked both checksums.
func gnuUnicodePAX(frames []byte, h *tar.Header) bool {
	if h.Format != tar.FormatUnknown || len(frames) < 1536 || len(h.PAXRecords) == 0 || frames[156] != tar.TypeXHeader {
		return false
	}
	extSize, err := strconv.ParseUint(strings.Trim(string(frames[124:136]), " \x00"), 8, 32)
	if err != nil || extSize == 0 || uint64(len(frames)) != 1024+(extSize+511)/512*512 {
		return false
	}
	last := frames[len(frames)-512:]
	unicodeName := false
	for _, b := range [][]byte{frames[:512], last} {
		if string(b[257:265]) != "ustar\x0000" {
			return false
		}
		for _, i := range []int{107, 115, 123, 135, 147, 336, 344} {
			if b[i] != 0 {
				return false
			}
		}
		for i, ch := range b {
			if ch < 128 {
				continue
			}
			if !(i < 100 || i >= 157 && i < 257 || i >= 345 && i < 500) {
				return false
			}
			unicodeName = true
		}
	}
	return unicodeName && (h.PAXRecords["path"] == h.Name || h.PAXRecords["linkpath"] != "" && h.PAXRecords["linkpath"] == h.Linkname)
}
