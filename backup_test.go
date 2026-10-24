package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const testPassword = "correct horse battery staple"

func write(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type fixture struct {
	src, share string
	photo      []byte
}

// newFixture builds a small "home folder" containing real data and plenty of junk.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{src: filepath.Join(root, "alice"), share: filepath.Join(root, "alice", "mnt", "share")}
	f.photo = randomBytes(300_000)
	write(t, filepath.Join(f.src, "Documents", "thesis.txt"), bytes.Repeat([]byte("lorem ipsum "), 5000), 0o644)
	write(t, filepath.Join(f.src, "Documents", "ünïcødé name.txt"), []byte("unicode"), 0o644)
	write(t, filepath.Join(f.src, "Pictures", "holiday.jpg"), f.photo, 0o644)
	write(t, filepath.Join(f.src, "projects", "api", "main.go"), []byte("package main"), 0o644)
	write(t, filepath.Join(f.src, "projects", "api", ".git", "HEAD"), []byte("ref: refs/heads/main"), 0o644)
	write(t, filepath.Join(f.src, "projects", "web", "package.json"), []byte("{}"), 0o644)
	write(t, filepath.Join(f.src, ".ssh", "id_ed25519"), []byte("PRIVATE KEY"), 0o600)
	write(t, filepath.Join(f.src, "empty-file"), nil, 0o644)
	if err := os.MkdirAll(filepath.Join(f.src, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// junk that must NOT be backed up
	write(t, filepath.Join(f.src, "projects", "web", "node_modules", "left-pad", "index.js"), randomBytes(5000), 0o644)
	write(t, filepath.Join(f.src, "projects", "api", ".venv", "lib", "python3.12", "site-packages", "x.py"), randomBytes(5000), 0o644)
	write(t, filepath.Join(f.src, "projects", "api", "__pycache__", "main.cpython-312.pyc"), randomBytes(500), 0o644)
	write(t, filepath.Join(f.src, ".cache", "thumbnails", "t.png"), randomBytes(5000), 0o644)
	write(t, filepath.Join(f.src, "go", "pkg", "mod", "github.com", "x", "y.go"), randomBytes(500), 0o644)
	write(t, filepath.Join(f.src, "Pictures", ".DS_Store"), []byte("x"), 0o644)
	// the destination share lives INSIDE the source (a classic mount-in-home setup)
	write(t, filepath.Join(f.src, "mnt", "readme.txt"), []byte("kept"), 0o644)
	write(t, filepath.Join(f.share, "other-backups-from-someone-else.zip"), randomBytes(1000), 0o644)

	if runtime.GOOS != "windows" {
		if err := os.Symlink("Documents/thesis.txt", filepath.Join(f.src, "link-to-thesis")); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	os.Chtimes(filepath.Join(f.src, "Documents", "thesis.txt"), old, old)
	return f
}

func (f *fixture) opts() backupOptions {
	return backupOptions{Sources: []string{f.src}, Share: f.share, Password: testPassword, Level: gzip.BestSpeed, Verify: true}
}

func TestBackupAndRestoreRoundTrip(t *testing.T) {
	lowCost(t)
	f := newFixture(t)
	ctx := context.Background()

	if err := runBackup(ctx, f.opts()); err != nil {
		t.Fatal(err)
	}

	backups, err := listBackups(f.share)
	if err != nil || len(backups) != 1 {
		t.Fatalf("listBackups = %v, %v", backups, err)
	}
	entries, _ := os.ReadDir(f.share)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".part" {
			t.Errorf("leftover partial file %s", e.Name())
		}
	}
	if !exists(filepath.Join(f.share, "other-backups-from-someone-else.zip")) {
		t.Error("an unrelated file on the share was touched")
	}

	archive, err := resolveArchive(f.share, "latest")
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out")
	if err := runRestore(ctx, restoreOptions{Archive: archive, Dest: dest, Password: testPassword}); err != nil {
		t.Fatal(err)
	}
	top := filepath.Join(dest, "alice")

	// content is identical
	for rel, want := range map[string][]byte{
		"Documents/thesis.txt":       readFile(t, filepath.Join(f.src, "Documents", "thesis.txt")),
		"Documents/ünïcødé name.txt": []byte("unicode"),
		"Pictures/holiday.jpg":       f.photo,
		"projects/api/main.go":       []byte("package main"),
		"projects/api/.git/HEAD":     []byte("ref: refs/heads/main"),
		".ssh/id_ed25519":            []byte("PRIVATE KEY"),
		"empty-file":                 {},
		"mnt/readme.txt":             []byte("kept"),
	} {
		if got := readFile(t, filepath.Join(top, filepath.FromSlash(rel))); !bytes.Equal(got, want) {
			t.Errorf("%s: content differs after restore", rel)
		}
	}
	if fi, err := os.Stat(filepath.Join(top, "empty-dir")); err != nil || !fi.IsDir() {
		t.Error("empty folder was not restored")
	}

	// junk is gone, .git is kept, and the share did not back itself up
	for _, rel := range []string{
		"projects/web/node_modules", "projects/api/.venv", "projects/api/__pycache__",
		".cache", "go/pkg/mod", "Pictures/.DS_Store", "mnt/share",
	} {
		if exists(filepath.Join(top, filepath.FromSlash(rel))) {
			t.Errorf("%s should not be in the backup", rel)
		}
	}

	if runtime.GOOS != "windows" {
		if target, err := os.Readlink(filepath.Join(top, "link-to-thesis")); err != nil || target != "Documents/thesis.txt" {
			t.Errorf("symlink = %q, %v", target, err)
		}
		if fi, _ := os.Stat(filepath.Join(top, ".ssh", "id_ed25519")); fi.Mode().Perm() != 0o600 {
			t.Errorf("private key permissions = %v, want 0600", fi.Mode().Perm())
		}
	}
	fi, _ := os.Stat(filepath.Join(top, "Documents", "thesis.txt"))
	if want := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC); !fi.ModTime().Equal(want) {
		t.Errorf("mtime = %v, want %v", fi.ModTime(), want)
	}

	// partial restore
	dest2 := filepath.Join(t.TempDir(), "partial")
	if err := runRestore(ctx, restoreOptions{Archive: archive, Dest: dest2, Only: "alice/Documents", Password: testPassword}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dest2, "alice", "Documents", "thesis.txt")) || exists(filepath.Join(dest2, "alice", "Pictures")) {
		t.Error("--only restored the wrong set of files")
	}

	// refuses to restore into a non-empty folder unless forced
	if err := runRestore(ctx, restoreOptions{Archive: archive, Dest: dest, Password: testPassword}); err == nil {
		t.Error("restoring into a non-empty folder should require --force")
	}
	if err := runRestore(ctx, restoreOptions{Archive: archive, Dest: dest, Password: testPassword, Force: true}); err != nil {
		t.Errorf("--force restore failed: %v", err)
	}

	// wrong password
	err = runRestore(ctx, restoreOptions{Archive: archive, Dest: filepath.Join(t.TempDir(), "x"), Password: "nope"})
	if !errors.Is(err, errAuth) {
		t.Errorf("wrong password: got %v", err)
	}
}

func TestCorruptedBackupFailsVerification(t *testing.T) {
	lowCost(t)
	f := newFixture(t)
	if err := runBackup(context.Background(), f.opts()); err != nil {
		t.Fatal(err)
	}
	archive, _ := resolveArchive(f.share, "latest")
	orig := readFile(t, archive)

	if _, err := verifyArchive(context.Background(), archive, testPassword); err != nil {
		t.Fatalf("pristine backup failed to verify: %v", err)
	}
	flipped := append([]byte(nil), orig...)
	flipped[len(flipped)/2] ^= 0x40
	os.WriteFile(archive, flipped, 0o600)
	if _, err := verifyArchive(context.Background(), archive, testPassword); err == nil {
		t.Error("a flipped byte was not detected")
	}
	os.WriteFile(archive, orig[:len(orig)-200], 0o600)
	if _, err := verifyArchive(context.Background(), archive, testPassword); err == nil {
		t.Error("a truncated backup was not detected")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	before, _ := os.ReadDir(f.share)
	o := f.opts()
	o.DryRun = true
	if err := runBackup(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadDir(f.share)
	if len(before) != len(after) {
		t.Fatal("dry run created files on the share")
	}
}

func TestCancelledBackupLeavesNothingBehind(t *testing.T) {
	lowCost(t)
	f := newFixture(t)
	before, _ := os.ReadDir(f.share)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runBackup(ctx, f.opts())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	after, _ := os.ReadDir(f.share)
	if len(before) != len(after) {
		t.Fatal("a cancelled backup left a file on the share")
	}
}

func TestBackupErrors(t *testing.T) {
	lowCost(t)
	f := newFixture(t)
	o := f.opts()
	o.Share = filepath.Join(t.TempDir(), "not-mounted")
	if err := runBackup(context.Background(), o); err == nil {
		t.Error("a missing share must be an error")
	}
	o = f.opts()
	o.Password = ""
	if err := runBackup(context.Background(), o); err == nil {
		t.Error("a missing password must be an error")
	}
	o = f.opts()
	o.Sources = []string{f.share}
	if err := runBackup(context.Background(), o); err == nil {
		t.Error("backing up the destination into itself must be an error")
	}
	o = f.opts()
	o.Sources = []string{filepath.Join(f.src, "nope")}
	if err := runBackup(context.Background(), o); err == nil {
		t.Error("a missing source must be an error")
	}
}

func TestOddFileNamesDoNotAbortTheBackup(t *testing.T) {
	lowCost(t)
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.src, "Documents", "bad\xffname.txt"), []byte("odd"), 0o644); err != nil {
		t.Skip("file system does not allow invalid UTF-8 names")
	}
	if err := runBackup(context.Background(), f.opts()); err != nil {
		t.Fatalf("backup aborted: %v", err)
	}
}

func TestUnreadableFileIsSkippedNotFatal(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a non-root unix user")
	}
	lowCost(t)
	f := newFixture(t)
	secret := filepath.Join(f.src, "Documents", "locked.txt")
	write(t, secret, []byte("x"), 0o000)
	if err := runBackup(context.Background(), f.opts()); err != nil {
		t.Fatalf("an unreadable file must not fail the whole backup: %v", err)
	}
}

// ---- hostile archives ------------------------------------------------------

func writeTestArchive(t *testing.T, path string, hdrs []*tar.Header, bodies map[string][]byte) {
	t.Helper()
	var buf bytes.Buffer
	enc, err := newEncryptWriter(&buf, testPassword)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	for _, h := range hdrs {
		body := bodies[h.Name]
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []io.Closer{tw, gz, enc} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreCannotEscapeDestination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ")
	}
	lowCost(t)
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	outside := filepath.Join(base, "outside")
	os.MkdirAll(outside, 0o755)

	hdrs := []*tar.Header{
		{Name: "../escape1.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "/tmp/ssmbak-abs-escape.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "a/../../escape2.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "top/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "top/link", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777},
		{Name: "top/link/pwned.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "top/rel", Typeflag: tar.TypeSymlink, Linkname: "../../outside", Mode: 0o777},
		{Name: "top/rel/pwned2.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "top/ok.txt", Typeflag: tar.TypeReg, Mode: 0o4755}, // setuid bit must be dropped
	}
	bodies := map[string][]byte{}
	for _, h := range hdrs {
		bodies[h.Name] = []byte("malicious")
	}
	archive := filepath.Join(base, "evil.tar.gz.enc")
	writeTestArchive(t, archive, hdrs, bodies)

	err := runRestore(context.Background(), restoreOptions{Archive: archive, Dest: dest, Password: testPassword})
	if err == nil {
		t.Error("restoring an archive with unsafe entries should report problems")
	}
	for _, p := range []string{
		filepath.Join(base, "escape1.txt"), filepath.Join(base, "escape2.txt"),
		"/tmp/ssmbak-abs-escape.txt", filepath.Join(outside, "pwned.txt"), filepath.Join(outside, "pwned2.txt"),
	} {
		if exists(p) {
			t.Errorf("SECURITY: %s was written outside the destination", p)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("SECURITY: outside folder got %d entries", len(entries))
	}
	fi, err2 := os.Stat(filepath.Join(dest, "top", "ok.txt"))
	if err2 != nil {
		t.Fatal("the safe entry should still be restored")
	}
	if fi.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		t.Error("setuid bit must not be restored")
	}
}
