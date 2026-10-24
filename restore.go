package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// readArchive streams a backup: decrypt -> gunzip -> untar, calling visit for
// every entry. Nothing is written to disk by this function, and it only
// succeeds if the whole file authenticated, including the final chunk.
func readArchive(ctx context.Context, file, password string, visit func(*tar.Header, io.Reader) error) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	dec, err := newDecryptReader(ctxReader{ctx, f}, password)
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(dec)
	if err != nil {
		if errors.Is(err, errAuth) {
			return errAuth
		}
		return fmt.Errorf("reading backup: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading backup: %w", err)
		}
		if err := visit(hdr, tr); err != nil {
			return err
		}
	}
	// tar stops at its end marker, before gzip's checksum and the last
	// encrypted chunk. Read to the very end so truncation can't go unnoticed.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return fmt.Errorf("reading backup: %w", err)
	}
	if _, err := io.Copy(io.Discard, dec); err != nil {
		return fmt.Errorf("reading backup: %w", err)
	}
	return nil
}

type verifyStats struct {
	entries int
	bytes   int64
}

func verifyArchive(ctx context.Context, file, password string) (verifyStats, error) {
	var vs verifyStats
	err := readArchive(ctx, file, password, func(h *tar.Header, r io.Reader) error {
		n, err := io.Copy(io.Discard, r)
		vs.entries++
		vs.bytes += n
		return err
	})
	return vs, err
}

// restore

type restoreOptions struct {
	Archive  string
	Dest     string
	Only     string // restore only this path inside the backup (and everything below it)
	Force    bool
	List     bool
	Password string
}

// resolveArchive turns --file into a path: "" or "latest" is the newest backup
// on the share, an existing path is used as is, otherwise it is a file name on
// the share.
func resolveArchive(share, sel string) (string, error) {
	if sel == "" || sel == "latest" {
		backups, err := listBackups(share)
		if err != nil {
			return "", fmt.Errorf("cannot read %q: %s", share, errText(err))
		}
		if len(backups) == 0 {
			return "", fmt.Errorf("no ssmbak backups found in %s", share)
		}
		return filepath.Join(share, backups[0].Name), nil
	}
	if fi, err := os.Stat(sel); err == nil && !fi.IsDir() {
		return sel, nil
	}
	p := filepath.Join(share, sel)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("backup %q not found (run `ssmbak list`)", sel)
	}
	return p, nil
}

func prepareDest(dest string, force bool) error {
	entries, err := os.ReadDir(dest)
	switch {
	case err == nil:
		if len(entries) > 0 && !force {
			return fmt.Errorf("%s is not empty; pick a new folder with --to, or use --force to restore into it (existing files are overwritten)", dest)
		}
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return os.MkdirAll(dest, 0o700)
	default:
		return err
	}
}

func normalizeOnly(s string) string {
	s = path.Clean(filepath.ToSlash(strings.TrimSpace(s)))
	s = strings.Trim(s, "/")
	if s == "." {
		return ""
	}
	return s
}

func runRestore(ctx context.Context, o restoreOptions) error {
	only := normalizeOnly(o.Only)
	if o.List {
		var n int
		err := readArchive(ctx, o.Archive, o.Password, func(h *tar.Header, _ io.Reader) error {
			name := strings.TrimSuffix(h.Name, "/")
			if only != "" && name != only && !strings.HasPrefix(name, only+"/") {
				return nil
			}
			n++
			if h.Typeflag == tar.TypeDir {
				fmt.Printf("%12s  %s/\n", "", name)
			} else {
				fmt.Printf("%12s  %s\n", humanBytes(h.Size), name)
			}
			return nil
		})
		if err != nil {
			return err
		}
		fmt.Printf("\n%d entries\n", n)
		return nil
	}

	if err := prepareDest(o.Dest, o.Force); err != nil {
		return err
	}
	// os.Root makes every file operation below incapable of leaving the
	// destination folder, even through ".." or symlinks stored in the archive.
	root, err := os.OpenRoot(o.Dest)
	if err != nil {
		return err
	}
	defer root.Close()

	x := &extractor{root: root, only: only, force: o.Force}
	start := time.Now()
	term.logf("restoring %s -> %s", filepath.Base(o.Archive), o.Dest)
	if err := readArchive(ctx, o.Archive, o.Password, x.extract); err != nil {
		return err
	}
	x.finishDirs()
	term.logf("restore complete: %d files (%s) in %s", x.files, humanBytes(x.bytes), humanDuration(time.Since(start)))
	if x.failed > 0 {
		return fmt.Errorf("%d entries could not be restored (see warnings above)", x.failed)
	}
	return nil
}

type dirMeta struct {
	name  string
	mode  fs.FileMode
	mtime time.Time
}

type extractor struct {
	root   *os.Root
	only   string
	force  bool
	dirs   []dirMeta
	files  int
	bytes  int64
	failed int
}

func (x *extractor) warn(name string, err error) {
	x.failed++
	term.logf("warning: cannot restore %s: %s", name, errText(err))
}

func (x *extractor) extract(h *tar.Header, r io.Reader) error {
	name := path.Clean(h.Name)
	if name == "." {
		return nil
	}
	if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
		x.warn(h.Name, errors.New("unsafe path in archive, skipped"))
		return nil
	}
	if x.only != "" && name != x.only && !strings.HasPrefix(name, x.only+"/") {
		return nil
	}
	mode := fs.FileMode(h.Mode).Perm() // drops setuid/setgid/sticky on purpose

	switch h.Typeflag {
	case tar.TypeDir:
		if err := x.root.MkdirAll(name, 0o755); err != nil {
			x.warn(name, err)
			return nil
		}
		x.dirs = append(x.dirs, dirMeta{name, mode, h.ModTime})

	case tar.TypeReg:
		if err := x.root.MkdirAll(path.Dir(name), 0o755); err != nil {
			x.warn(name, err)
			return nil
		}
		f, err := x.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode|0o200)
		if err != nil {
			x.warn(name, err)
			return nil
		}
		n, err := io.Copy(f, r)
		cerr := f.Close()
		if err != nil {
			return fmt.Errorf("restoring %s: %w", name, err) // bad stream or full disk: stop
		}
		if cerr != nil {
			x.warn(name, cerr)
			return nil
		}
		x.files++
		x.bytes += n
		_ = x.root.Chmod(name, mode)
		_ = x.root.Chtimes(name, time.Time{}, h.ModTime)

	case tar.TypeSymlink:
		if err := x.root.MkdirAll(path.Dir(name), 0o755); err != nil {
			x.warn(name, err)
			return nil
		}
		if x.force {
			_ = x.root.Remove(name)
		}
		if err := x.root.Symlink(h.Linkname, name); err != nil {
			x.warn(name, err)
		}

	default:
		x.warn(name, fmt.Errorf("unsupported entry type %q, skipped", h.Typeflag))
	}
	return nil
}

// finishDirs applies folder permissions and times last, deepest first, so that
// a read-only folder doesn't block restoring what's inside it.
func (x *extractor) finishDirs() {
	for i := len(x.dirs) - 1; i >= 0; i-- {
		d := x.dirs[i]
		_ = x.root.Chmod(d.name, d.mode)
		_ = x.root.Chtimes(d.name, time.Time{}, d.mtime)
	}
}
