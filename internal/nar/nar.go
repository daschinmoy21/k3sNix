// Package nar writes Nix archives (NAR) from filesystem paths.
package nar

import (
	"bufio"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
)

const magic = "nix-archive-1"

// Write serializes root as a NAR stream to w. Symlinks are recorded as links
// and never followed.
func Write(w io.Writer, root string) error {
	bw := bufio.NewWriter(w)
	if err := writeString(bw, magic); err != nil {
		return err
	}
	if err := writeNode(bw, root); err != nil {
		return err
	}
	return bw.Flush()
}

func writeNode(w io.Writer, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}

	if err := writeString(w, "("); err != nil {
		return err
	}
	if err := writeString(w, "type"); err != nil {
		return err
	}

	switch {
	case info.Mode()&os.ModeSymlink != 0:
		if err := writeSymlink(w, path); err != nil {
			return err
		}
	case info.IsDir():
		if err := writeDirectory(w, path); err != nil {
			return err
		}
	default:
		if err := writeRegular(w, path, info); err != nil {
			return err
		}
	}

	return writeString(w, ")")
}

func writeSymlink(w io.Writer, path string) error {
	target, err := os.Readlink(path)
	if err != nil {
		return err
	}
	if err := writeString(w, "symlink"); err != nil {
		return err
	}
	if err := writeString(w, "target"); err != nil {
		return err
	}
	return writeString(w, target)
}

func writeDirectory(w io.Writer, path string) error {
	if err := writeString(w, "directory"); err != nil {
		return err
	}
	// os.ReadDir sorts by filename, which is the byte order NAR requires.
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := writeString(w, "entry"); err != nil {
			return err
		}
		if err := writeString(w, "("); err != nil {
			return err
		}
		if err := writeString(w, "name"); err != nil {
			return err
		}
		if err := writeString(w, entry.Name()); err != nil {
			return err
		}
		if err := writeString(w, "node"); err != nil {
			return err
		}
		if err := writeNode(w, filepath.Join(path, entry.Name())); err != nil {
			return err
		}
		if err := writeString(w, ")"); err != nil {
			return err
		}
	}
	return nil
}

func writeRegular(w io.Writer, path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return &os.PathError{Op: "Write", Path: path, Err: os.ErrInvalid}
	}
	if err := writeString(w, "regular"); err != nil {
		return err
	}
	if info.Mode()&0o111 != 0 {
		if err := writeString(w, "executable"); err != nil {
			return err
		}
		if err := writeString(w, ""); err != nil {
			return err
		}
	}
	if err := writeString(w, "contents"); err != nil {
		return err
	}

	f, err := openNoFollow(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return &os.PathError{Op: "Write", Path: path, Err: os.ErrInvalid}
	}
	return writeContents(w, f, fi.Size())
}

func writeContents(w io.Writer, r io.Reader, size int64) error {
	if err := writeUint64(w, uint64(size)); err != nil {
		return err
	}
	if _, err := io.CopyN(w, r, size); err != nil {
		return err
	}
	return writePad(w, size)
}

func writeString(w io.Writer, s string) error {
	if err := writeUint64(w, uint64(len(s))); err != nil {
		return err
	}
	if _, err := io.WriteString(w, s); err != nil {
		return err
	}
	return writePad(w, int64(len(s)))
}

func writeUint64(w io.Writer, v uint64) error {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], v)
	_, err := w.Write(buf[:])
	return err
}

func writePad(w io.Writer, n int64) error {
	pad := (8 - n%8) % 8
	if pad == 0 {
		return nil
	}
	var zeros [8]byte
	_, err := w.Write(zeros[:pad])
	return err
}
