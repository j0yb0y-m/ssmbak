package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

const (
	backupPrefix = "ssmbak_backup_"
	backupSuffix = ".tar.gz.enc"
	timeLayout   = "20060102_150405"
)

var (
	backupNameRe = regexp.MustCompile(`^ssmbak_backup_(\d{8}_\d{6})\.tar\.gz\.enc$`)
	partNameRe   = regexp.MustCompile(`^ssmbak_backup_\d{8}_\d{6}\.tar\.gz\.enc\.part$`)
)

type backupFile struct {
	Name string
	Time time.Time
	Size int64
}

// listBackups returns the backups on the share, newest first. Anything that
// doesn't match ssmbak's exact naming scheme is ignored, so ssmbak can never
// touch other files that live on the same share.
func listBackups(share string) ([]backupFile, error) {
	entries, err := os.ReadDir(share)
	if err != nil {
		return nil, err
	}
	var out []backupFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := backupNameRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		t, err := time.ParseInLocation(timeLayout, m[1], time.Local)
		if err != nil {
			continue
		}
		var size int64
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		out = append(out, backupFile{Name: e.Name(), Time: t, Size: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

// selectPrunable picks the backups to delete from a newest-first list: those
// older than keepDays that are not among the keepMin newest. The newest backup
// is never selected, whatever the settings. keepDays <= 0 means "keep forever".
func selectPrunable(backups []backupFile, keepDays, keepMin int, now time.Time) []backupFile {
	if keepDays <= 0 {
		return nil
	}
	if keepMin < 1 {
		keepMin = 1
	}
	cutoff := now.AddDate(0, 0, -keepDays)
	var out []backupFile
	for i, b := range backups {
		if i >= keepMin && b.Time.Before(cutoff) {
			out = append(out, b)
		}
	}
	return out
}

// pruneBackups is only called after a successful backup, so a run that fails
// never shrinks the set of backups you already have.
func pruneBackups(share string, keepDays, keepMin int, now time.Time) {
	removeStaleParts(share, now)
	backups, err := listBackups(share)
	if err != nil {
		term.logf("warning: cannot list backups for cleanup: %v", err)
		return
	}
	for _, b := range selectPrunable(backups, keepDays, keepMin, now) {
		if err := os.Remove(filepath.Join(share, b.Name)); err != nil {
			term.logf("warning: could not delete old backup %s: %v", b.Name, errText(err))
			continue
		}
		term.logf("deleted old backup %s", b.Name)
	}
}

// removeStaleParts deletes leftovers of uploads that crashed (.part files that
// have not been touched for a day).
func removeStaleParts(share string, now time.Time) {
	entries, err := os.ReadDir(share)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !partNameRe.MatchString(e.Name()) {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > 24*time.Hour {
			if os.Remove(filepath.Join(share, e.Name())) == nil {
				term.logf("removed stale partial upload %s", e.Name())
			}
		}
	}
}
