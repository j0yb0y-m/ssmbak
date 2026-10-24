package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDotenv(t *testing.T) {
	in := "# comment\n\nSMB_SHARE_PATH=/mnt/nas\nexport BACKUP_PASSWORD=\"pa ss#word\"\nA = 'b'\n  C=d=e  \n"
	got, err := parseDotenv(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"SMB_SHARE_PATH": "/mnt/nas", "BACKUP_PASSWORD": "pa ss#word", "A": "b", "C": "d=e"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, err := parseDotenv(strings.NewReader("just some words\n")); err == nil {
		t.Error("a line without '=' should be an error")
	}
}

func setHome(t *testing.T, dir string) {
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

func TestLoadConfigPrecedenceAndDefaults(t *testing.T) {
	dir := t.TempDir()
	setHome(t, dir)
	for _, k := range []string{"SMB_SHARE_PATH", "BACKUP_PASSWORD", "SSMBAK_SOURCE", "SSMBAK_EXCLUDE", "SSMBAK_RETENTION_DAYS", "SSMBAK_KEEP_MIN", "SSMBAK_PASSWORD_FILE", "SSMBAK_COMPRESSION"} {
		t.Setenv(k, "")
	}
	cfgFile := filepath.Join(dir, "c.env")
	sep := string(os.PathListSeparator)
	content := "SMB_SHARE_PATH=/from/file\nBACKUP_PASSWORD=filepw-filepw\nSSMBAK_SOURCE=~/Documents" + sep + "~/Pictures\nSSMBAK_EXCLUDE=target,*.iso\nSSMBAK_RETENTION_DAYS=7\nSSMBAK_SOURC=typo\n"
	if err := os.WriteFile(cfgFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SMB_SHARE_PATH", "/from/env") // environment beats the file

	c, err := loadConfig(cfgFile, true)
	if err != nil {
		t.Fatal(err)
	}
	if c.Share != "/from/env" {
		t.Errorf("share = %q, want env value", c.Share)
	}
	if c.Password != "filepw-filepw" || c.KeepDays != 7 || c.KeepMin != 3 || c.Compress != "fast" {
		t.Errorf("unexpected config: %+v", c)
	}
	if len(c.Sources) != 2 || c.Sources[0] != filepath.Join(dir, "Documents") {
		t.Errorf("sources = %v (~ should expand to the home folder)", c.Sources)
	}
	if len(c.Excludes) != 2 {
		t.Errorf("excludes = %v", c.Excludes)
	}

	if _, err := loadConfig(filepath.Join(dir, "missing.env"), true); err == nil {
		t.Error("an explicitly requested config file that is missing must be an error")
	}
	if _, err := loadConfig(filepath.Join(dir, "missing.env"), false); err != nil {
		t.Errorf("a missing default config file must not be an error: %v", err)
	}
	t.Setenv("SSMBAK_RETENTION_DAYS", "soon")
	if _, err := loadConfig(cfgFile, true); err == nil {
		t.Error("a non-numeric retention must be an error")
	}
}

func TestPasswordFile(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(dir, "pw")
	os.WriteFile(pf, []byte("s3cret-from-file\n"), 0o600)
	t.Setenv("BACKUP_PASSWORD", "")
	t.Setenv("SSMBAK_PASSWORD_FILE", pf)
	c, err := loadConfig("", false)
	if err != nil {
		t.Fatal(err)
	}
	if c.Password != "s3cret-from-file" {
		t.Errorf("password = %q", c.Password)
	}
}
