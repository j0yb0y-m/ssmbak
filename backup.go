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
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

type backupOptions struct {
	Sources  []string
	Share    string
	Password string
	Excludes []string // extra patterns, on top of defaultExcludes
	Level    int      // gzip level
	DryRun   bool
	Verbose  bool
	Verify   bool
	KeepDays int
	KeepMin  int
}

type stats struct {
	files, dirs, links, skipped, excluded atomic.Int64
	bytesIn, bytesOut                     atomic.Int64
}

// source is one folder to back up. Inside the archive it appears as a top-level
// folder called top, e.g. "mahdi/Documents/...".
type source struct{ root, top string }

type entry struct {
	path string // absolute path on disk
	name string // slash-separated path inside the archive
	info fs.FileInfo
}

func parseCompression(s string) (int, error) {
	switch strings.ToLower(s) {
	case "none":
		return gzip.NoCompression, nil
	case "fast", "":
		return gzip.BestSpeed, nil
	case "default":
		return gzip.DefaultCompression, nil
	case "best":
		return gzip.BestCompression, nil
	}
	return 0, fmt.Errorf("unknown compression %q (use none, fast, default or best)", s)
}

// errText drops the repeated path from *PathError messages.
func errText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

func resolveSources(paths []string) ([]source, error) {
	var out []source
	seen := map[string]string{}
	for _, p := range paths {
		abs, err := filepath.Abs(expandHome(p))
		if err != nil {
			return nil, err
		}
		fi, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("source folder: %w", err)
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("source %s is not a folder", abs)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, err
		}
		top := filepath.Base(abs)
		if top == string(filepath.Separator) || top == "." || top == "" {
			top = "root"
		}
		if prev, dup := seen[top]; dup {
			return nil, fmt.Errorf("sources %s and %s are both called %q; back them up separately", prev, abs, top)
		}
		seen[top] = abs
		out = append(out, source{root: real, top: top})
	}
	return out, nil
}

// walking

type walker struct {
	ctx       context.Context
	srcs      []source
	match     *matcher
	shareInfo os.FileInfo // the destination folder; never backed up (it may live inside the source)
	st        *stats
	onExclude func(name, rule string)
}

// walk visits every file, folder and symlink that should be backed up, in a
// stable order. Problems with individual files (permission denied, vanished
// files) are logged and skipped; only fatal errors are returned.
func (w *walker) walk(visit func(entry) error) error {
	for _, s := range w.srcs {
		err := filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if d == nil {
					return err // the source folder itself is unreadable
				}
				w.st.skipped.Add(1)
				term.logf("warning: skipping %s: %s", p, errText(err))
				return nil
			}
			if cerr := w.ctx.Err(); cerr != nil {
				return cerr
			}
			rel, rerr := filepath.Rel(s.root, p)
			if rerr != nil {
				return rerr
			}
			name := s.top
			if rel != "." {
				slashRel := filepath.ToSlash(rel)
				if rule, ok := w.match.match(slashRel); ok {
					w.st.excluded.Add(1)
					if w.onExclude != nil {
						w.onExclude(s.top+"/"+slashRel, rule)
					}
					if d.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				name = s.top + "/" + slashRel
			}
			info, ierr := d.Info()
			if ierr != nil {
				w.st.skipped.Add(1)
				term.logf("warning: skipping %s: %s", p, errText(ierr))
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			switch {
			case info.IsDir():
				if w.shareInfo != nil && os.SameFile(info, w.shareInfo) {
					term.logf("skipping %s: it is the backup destination", p)
					return fs.SkipDir
				}
			case info.Mode().IsRegular(), info.Mode()&fs.ModeSymlink != 0:
			default: // sockets, pipes, devices
				w.st.skipped.Add(1)
				return nil
			}
			return visit(entry{path: p, name: name, info: info})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *walker) dryRun(verbose bool) error {
	var total int64
	w.onExclude = func(name, rule string) {
		fmt.Printf("exclude  %s   [%s]\n", name, rule)
	}
	err := w.walk(func(e entry) error {
		switch {
		case e.info.IsDir():
			w.st.dirs.Add(1)
		case e.info.Mode().IsRegular():
			w.st.files.Add(1)
			total += e.info.Size()
			if verbose {
				fmt.Printf("include  %s   (%s)\n", e.name, humanBytes(e.info.Size()))
			}
		default:
			w.st.links.Add(1)
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("\nDry run: would back up %d files (%s) in %d folders, plus %d symlinks.\n",
		w.st.files.Load(), humanBytes(total), w.st.dirs.Load(), w.st.links.Load())
	fmt.Printf("Excluded %d files/folders, skipped %d unreadable or special files. Nothing was written.\n",
		w.st.excluded.Load(), w.st.skipped.Load())
	return nil
}

// archiving

var zeros [32 * 1024]byte

type archiver struct {
	ctx context.Context
	tw  *tar.Writer
	st  *stats
	buf []byte
}

func (a *archiver) skip(e entry, err error) {
	a.st.skipped.Add(1)
	term.logf("warning: skipping %s: %s", e.path, errText(err))
}

// writeHeader writes a tar header. A header that tar can't encode (for example
// a file name that isn't valid UTF-8) only skips that one entry; an I/O error
// on the destination is returned and aborts the backup.
func (a *archiver) writeHeader(e entry, hdr *tar.Header) (bool, error) {
	hdr.Uname, hdr.Gname = "", ""
	if err := a.tw.WriteHeader(hdr); err != nil {
		if ferr := a.tw.Flush(); ferr != nil {
			return false, ferr
		}
		a.skip(e, err)
		return false, nil
	}
	return true, nil
}

func (a *archiver) add(e entry) error {
	if err := a.ctx.Err(); err != nil {
		return err
	}
	switch mode := e.info.Mode(); {
	case mode.IsDir():
		hdr, err := tar.FileInfoHeader(e.info, "")
		if err != nil {
			a.skip(e, err)
			return nil
		}
		hdr.Name = e.name + "/"
		if ok, err := a.writeHeader(e, hdr); ok {
			a.st.dirs.Add(1)
		} else if err != nil {
			return err
		}
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(e.path)
		if err != nil {
			a.skip(e, err)
			return nil
		}
		hdr, err := tar.FileInfoHeader(e.info, target)
		if err != nil {
			a.skip(e, err)
			return nil
		}
		hdr.Name = e.name
		if ok, err := a.writeHeader(e, hdr); ok {
			a.st.links.Add(1)
		} else if err != nil {
			return err
		}
	default:
		return a.addFile(e)
	}
	return nil
}

func (a *archiver) addFile(e entry) error {
	f, err := os.Open(e.path)
	if err != nil {
		a.skip(e, err)
		return nil
	}
	defer f.Close()
	// Stat the open file (not the path) so the size we record is the size we read.
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		a.skip(e, errors.New("changed while backing up"))
		return nil
	}
	hdr, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		a.skip(e, err)
		return nil
	}
	hdr.Name = e.name
	if ok, err := a.writeHeader(e, hdr); !ok {
		return err
	}

	// tar needs exactly hdr.Size bytes. If the file grows we ignore the extra;
	// if it shrinks or a read fails we pad with zeros and warn, so the archive
	// always stays valid and one bad file can't ruin the whole backup.
	remaining := hdr.Size
	var readErr error
	for remaining > 0 {
		if err := a.ctx.Err(); err != nil {
			return err
		}
		chunk := a.buf[:int(min(int64(len(a.buf)), remaining))]
		n, rerr := f.Read(chunk)
		if n > 0 {
			if _, werr := a.tw.Write(chunk[:n]); werr != nil {
				return werr
			}
			remaining -= int64(n)
			a.st.bytesIn.Add(int64(n))
		}
		if rerr != nil {
			if rerr != io.EOF {
				readErr = rerr
			}
			break
		}
	}
	if remaining > 0 {
		reason := "file shrank while it was being read"
		if readErr != nil {
			reason = errText(readErr)
		}
		term.logf("warning: %s: %s; the last %s are stored as zeros", e.path, reason, humanBytes(remaining))
		for remaining > 0 {
			n := int(min(int64(len(zeros)), remaining))
			if _, werr := a.tw.Write(zeros[:n]); werr != nil {
				return werr
			}
			remaining -= int64(n)
		}
		a.st.skipped.Add(1)
	}
	a.st.files.Add(1)
	return nil
}

// the backup pipeline

func runBackup(ctx context.Context, o backupOptions) error {
	srcs, err := resolveSources(o.Sources)
	if err != nil {
		return err
	}
	m, err := newMatcher(defaultExcludes, o.Excludes)
	if err != nil {
		return err
	}
	shareInfo, err := os.Stat(o.Share)
	if err == nil && !shareInfo.IsDir() {
		err = errors.New("not a folder")
	}
	if err != nil {
		if !o.DryRun {
			return fmt.Errorf("backup destination %q is not accessible: %s\nIs the SMB share mounted? See the README, section \"Mounting the share\"", o.Share, errText(err))
		}
		term.logf("warning: destination %q is not accessible (%s); continuing because this is a dry run", o.Share, errText(err))
		shareInfo = nil
	}
	if shareInfo != nil {
		for _, s := range srcs {
			if fi, err := os.Stat(s.root); err == nil && os.SameFile(fi, shareInfo) {
				return fmt.Errorf("the source folder %s is the backup destination", s.root)
			}
		}
	}
	w := &walker{ctx: ctx, srcs: srcs, match: m, shareInfo: shareInfo, st: &stats{}}
	if o.DryRun {
		return w.dryRun(o.Verbose)
	}
	if o.Password == "" {
		return errors.New("no password set: set BACKUP_PASSWORD or SSMBAK_PASSWORD_FILE (run `ssmbak init` to create one)")
	}

	start := time.Now()
	name := backupPrefix + start.Format(timeLayout) + backupSuffix
	final := filepath.Join(o.Share, name)
	part := final + ".part"

	// Write to a .part file and rename when complete: the share never contains
	// something that looks like a finished backup but isn't.
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("cannot create %s: %s", part, errText(err))
	}
	fail := func(err error) error {
		f.Close()
		os.Remove(part)
		return err
	}

	names := make([]string, 0, len(srcs))
	for _, s := range srcs {
		names = append(names, s.top)
	}
	term.logf("backing up %s -> %s", strings.Join(names, ", "), final)

	stop := startProgress(w.st, start)
	err = writeArchive(ctx, f, o, w)
	stop()
	if err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(fmt.Errorf("flushing %s to the share: %w", name, err))
	}
	if err := f.Close(); err != nil {
		return fail(fmt.Errorf("closing %s: %w", name, err))
	}
	if err := os.Rename(part, final); err != nil {
		return fail(fmt.Errorf("finishing %s: %s", name, errText(err)))
	}

	elapsed := time.Since(start)
	in := w.st.bytesIn.Load()
	rate := float64(in) / max(elapsed.Seconds(), 0.001)
	term.logf("backup complete: %s", name)
	term.logf("  %d files, %d folders, %d symlinks; %s read, %s written in %s (%s/s)",
		w.st.files.Load(), w.st.dirs.Load(), w.st.links.Load(),
		humanBytes(in), humanBytes(w.st.bytesOut.Load()), humanDuration(elapsed), humanBytes(int64(rate)))
	term.logf("  %d excluded as junk", w.st.excluded.Load())
	if n := w.st.skipped.Load(); n > 0 {
		term.logf("  WARNING: %d files could not be read or were skipped (see warnings above)", n)
	}

	if o.Verify {
		term.logf("verifying %s ...", name)
		vs, err := verifyArchive(ctx, final, o.Password)
		if err != nil {
			return fmt.Errorf("VERIFY FAILED for %s: %w", name, err)
		}
		term.logf("verified: all %d entries decrypt and unpack correctly", vs.entries)
	}

	pruneBackups(o.Share, o.KeepDays, o.KeepMin, time.Now())
	return nil
}

// writeArchive builds the pipeline
//
//	files -> tar -> gzip -> encrypt (AES-256-GCM) -> async queue -> share
//
// in a single pass, with no temporary files and a few MiB of memory.
func writeArchive(ctx context.Context, f *os.File, o backupOptions, w *walker) error {
	aw := newAsyncWriter(&countWriter{w: f, n: &w.st.bytesOut}, 16)
	enc, err := newEncryptWriter(aw, o.Password)
	if err != nil {
		aw.Close()
		return err
	}
	gz, err := gzip.NewWriterLevel(enc, o.Level)
	if err != nil {
		aw.Close()
		return err
	}
	tw := tar.NewWriter(gz)
	a := &archiver{ctx: ctx, tw: tw, st: w.st, buf: make([]byte, 1<<20)}

	if err := w.walk(a.add); err != nil {
		aw.Close()
		return err
	}
	for _, closeStage := range []func() error{tw.Close, gz.Close, enc.Close, aw.Close} {
		if err := closeStage(); err != nil {
			aw.Close()
			return err
		}
	}
	return nil
}

func startProgress(st *stats, start time.Time) (stop func()) {
	if !term.tty {
		return func() {}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				in := st.bytesIn.Load()
				rate := float64(in) / max(time.Since(start).Seconds(), 0.001)
				term.status(fmt.Sprintf("%d files, %s read, %s/s", st.files.Load(), humanBytes(in), humanBytes(int64(rate))))
			}
		}
	}()
	return func() {
		close(done)
		<-finished
		term.clear()
	}
}
