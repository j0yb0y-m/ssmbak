package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mkBackup(now time.Time, daysOld int) backupFile {
	ts := now.AddDate(0, 0, -daysOld)
	return backupFile{Name: backupPrefix + ts.Format(timeLayout) + backupSuffix, Time: ts}
}

func TestSelectPrunable(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.Local)
	// newest first: 1, 5, 20, 31, 40, 90 days old
	var all []backupFile
	for _, d := range []int{1, 5, 20, 31, 40, 90} {
		all = append(all, mkBackup(now, d))
	}
	cases := []struct {
		name              string
		days, min         int
		wantDaysOldPruned []int
	}{
		{"30 days keep 3", 30, 3, []int{31, 40, 90}},
		{"keep-min protects the 5 newest", 30, 5, []int{90}},
		{"keep-min 0 still keeps the newest", 1, 0, []int{5, 20, 31, 40, 90}},
		{"retention 0 never deletes", 0, 3, nil},
		{"nothing old enough", 365, 1, nil},
	}
	for _, c := range cases {
		got := selectPrunable(all, c.days, c.min, now)
		if len(got) != len(c.wantDaysOldPruned) {
			t.Errorf("%s: pruned %d, want %d", c.name, len(got), len(c.wantDaysOldPruned))
			continue
		}
		for i, d := range c.wantDaysOldPruned {
			if want := mkBackup(now, d).Name; got[i].Name != want {
				t.Errorf("%s: pruned[%d]=%s, want %s", c.name, i, got[i].Name, want)
			}
		}
	}
}

func TestListBackupsIgnoresForeignFiles(t *testing.T) {
	share := t.TempDir()
	now := time.Now()
	good := mkBackup(now, 2).Name
	for _, name := range []string{
		good,
		"holiday-photos.zip", "ssmbak_backup_notadate.tar.gz.enc", "ssmbak_backup_20260101_000000.zip.enc",
		"ssmbak_backup_20260101_000000.tar.gz.enc.part", "my_ssmbak_backup_20260101_000000.tar.gz.enc",
	} {
		if err := os.WriteFile(filepath.Join(share, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	os.Mkdir(filepath.Join(share, "ssmbak_backup_20250101_000000.tar.gz.enc"), 0o700) // a folder must be ignored
	got, err := listBackups(share)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != good {
		t.Fatalf("listBackups = %+v, want only %s", got, good)
	}
}

func TestPruneOnlyTouchesOwnFiles(t *testing.T) {
	share := t.TempDir()
	now := time.Now()
	write := func(name string, age time.Duration) {
		p := filepath.Join(share, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := now.Add(-age)
		os.Chtimes(p, mt, mt)
	}
	for _, d := range []int{1, 40, 50, 60} {
		write(mkBackup(now, d).Name, 0)
	}
	write("family-photos.zip", 400*24*time.Hour) // not ours: must survive
	stale := "ssmbak_backup_20250101_000000.tar.gz.enc.part"
	write(stale, 48*time.Hour)
	fresh := "ssmbak_backup_20261024_110000.tar.gz.enc.part"
	write(fresh, time.Minute)

	pruneBackups(share, 30, 2, now)

	has := func(name string) bool { _, err := os.Stat(filepath.Join(share, name)); return err == nil }
	if !has(mkBackup(now, 1).Name) || !has(mkBackup(now, 40).Name) {
		t.Error("the two newest backups must be kept")
	}
	if has(mkBackup(now, 50).Name) || has(mkBackup(now, 60).Name) {
		t.Error("old backups beyond keep-min should be deleted")
	}
	if !has("family-photos.zip") {
		t.Error("a foreign file was deleted!")
	}
	if has(stale) {
		t.Error("stale .part should be removed")
	}
	if !has(fresh) {
		t.Error("a recent .part (maybe an upload in progress) must not be removed")
	}
}
