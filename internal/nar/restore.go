package nar

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxToken bounds a NAR string that is not file contents: a keyword, an
// entry name or a symlink target.
const maxToken = 64 << 10

// Restore unpacks the NAR stream r into dst, which must not exist. Regular
// files are created read-only (0444, or 0555 when executable) like a store
// path; directories are left writable by their owner so a failed restore
// can be removed. Entry names that are empty, ".", "..", or contain "/" or
// NUL are rejected, as are entries out of order and trailing bytes. On an
// error, whatever was written under dst is left for the caller to remove.
func Restore(r io.Reader, dst string) error {
	d := &decoder{r: bufio.NewReader(r)}
	if err := d.expect(magic); err != nil {
		return err
	}
	if err := d.node(dst); err != nil {
		return err
	}
	if _, err := d.r.ReadByte(); err != io.EOF {
		return errors.New("nar: trailing data after the archive")
	}
	return nil
}

type decoder struct {
	r *bufio.Reader
}

func (d *decoder) uint64() (uint64, error) {
	var buf [8]byte
	if _, err := io.ReadFull(d.r, buf[:]); err != nil {
		return 0, unexpected(err)
	}
	return binary.LittleEndian.Uint64(buf[:]), nil
}

func (d *decoder) pad(n uint64) error {
	pad := (8 - n%8) % 8
	var buf [8]byte
	if _, err := io.ReadFull(d.r, buf[:pad]); err != nil {
		return unexpected(err)
	}
	for _, b := range buf[:pad] {
		if b != 0 {
			return errors.New("nar: non-zero padding")
		}
	}
	return nil
}

func (d *decoder) token() (string, error) {
	n, err := d.uint64()
	if err != nil {
		return "", err
	}
	if n > maxToken {
		return "", fmt.Errorf("nar: %d-byte string exceeds %d", n, maxToken)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(d.r, buf); err != nil {
		return "", unexpected(err)
	}
	return string(buf), d.pad(n)
}

func (d *decoder) expect(want string) error {
	got, err := d.token()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("nar: got %q, want %q", got, want)
	}
	return nil
}

func (d *decoder) node(path string) error {
	if err := d.expect("("); err != nil {
		return err
	}
	if err := d.expect("type"); err != nil {
		return err
	}
	typ, err := d.token()
	if err != nil {
		return err
	}
	switch typ {
	case "regular":
		err = d.regular(path)
	case "symlink":
		err = d.symlink(path)
	case "directory":
		// The entry loop ends on the node's closing paren.
		return d.directory(path)
	default:
		err = fmt.Errorf("nar: unknown node type %q", typ)
	}
	if err != nil {
		return err
	}
	return d.expect(")")
}

func (d *decoder) regular(path string) error {
	tok, err := d.token()
	if err != nil {
		return err
	}
	mode := os.FileMode(0o444)
	if tok == "executable" {
		if err := d.expect(""); err != nil {
			return err
		}
		mode = 0o555
		if tok, err = d.token(); err != nil {
			return err
		}
	}
	if tok != "contents" {
		return fmt.Errorf("nar: got %q, want \"contents\"", tok)
	}
	size, err := d.uint64()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(f, d.r, int64(size)); err != nil {
		f.Close()
		return unexpected(err)
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return d.pad(size)
}

func (d *decoder) symlink(path string) error {
	if err := d.expect("target"); err != nil {
		return err
	}
	target, err := d.token()
	if err != nil {
		return err
	}
	if target == "" || strings.IndexByte(target, 0) >= 0 {
		return fmt.Errorf("nar: bad symlink target %q", target)
	}
	return os.Symlink(target, path)
}

func (d *decoder) directory(path string) error {
	if err := os.Mkdir(path, 0o755); err != nil {
		return err
	}
	prev := ""
	for {
		tok, err := d.token()
		if err != nil {
			return err
		}
		if tok == ")" {
			return nil
		}
		if tok != "entry" {
			return fmt.Errorf("nar: got %q, want \"entry\"", tok)
		}
		if err := d.expect("("); err != nil {
			return err
		}
		if err := d.expect("name"); err != nil {
			return err
		}
		name, err := d.token()
		if err != nil {
			return err
		}
		if !validName(name) {
			return fmt.Errorf("nar: bad entry name %q", name)
		}
		if prev != "" && name <= prev {
			return fmt.Errorf("nar: entry %q after %q is out of order", name, prev)
		}
		prev = name
		if err := d.expect("node"); err != nil {
			return err
		}
		if err := d.node(filepath.Join(path, name)); err != nil {
			return err
		}
		if err := d.expect(")"); err != nil {
			return err
		}
	}
}

func validName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, "/\x00")
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
