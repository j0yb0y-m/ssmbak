package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Settings can come from, in increasing priority: the config file, environment
// variables, command-line flags. The names are the same in the file and the
// environment (the file is just KEY=VALUE lines).
var knownKeys = map[string]bool{
	"SMB_SHARE_PATH":        true, // folder where backups are written (a mounted SMB share)
	"BACKUP_PASSWORD":       true, // encryption password
	"SSMBAK_PASSWORD_FILE":  true, // alternative: read the password from this file
	"SSMBAK_SOURCE":         true, // folder(s) to back up, separated by ':' (';' on Windows)
	"SSMBAK_EXCLUDE":        true, // extra exclude patterns, comma-separated
	"SSMBAK_RETENTION_DAYS": true, // delete backups older than this (0 = never)
	"SSMBAK_KEEP_MIN":       true, // ...but always keep at least this many
	"SSMBAK_COMPRESSION":    true, // none | fast | default | best
}

type config struct {
	Sources  []string
	Share    string
	Password string
	Excludes []string
	KeepDays int
	KeepMin  int
	Compress string
}

func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "ssmbak", "config.env")
}

// configPathFor decides which config file to read. mustExist is true when the
// user asked for a specific file (flag or SSMBAK_CONFIG).
func configPathFor(flagValue string) (path string, mustExist bool) {
	if flagValue != "" {
		return flagValue, true
	}
	if env := os.Getenv("SSMBAK_CONFIG"); env != "" {
		return env, true
	}
	return defaultConfigPath(), false
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

func parseDotenv(r io.Reader) (map[string]string, error) {
	m := map[string]string{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", n)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		// Values are taken literally (a '#' inside a password is fine);
		// surrounding quotes are optional.
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		m[k] = v
	}
	return m, sc.Err()
}

func loadConfig(path string, mustExist bool) (*config, error) {
	file := map[string]string{}
	if path != "" {
		f, err := os.Open(path)
		switch {
		case err == nil:
			fi, _ := f.Stat()
			file, err = parseDotenv(f)
			f.Close()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			for k := range file {
				if !knownKeys[k] {
					term.logf("warning: %s: unknown setting %q is ignored (typo?)", path, k)
				}
			}
			_, hasPw := file["BACKUP_PASSWORD"]
			if hasPw && fi != nil && runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
				term.logf("warning: %s contains your backup password but is readable by other users; run: chmod 600 %s", path, path)
			}
		case errors.Is(err, fs.ErrNotExist) && !mustExist:
			// No config file is fine: environment variables and flags still work.
		default:
			return nil, err
		}
	}
	get := func(key string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return file[key]
	}

	c := &config{
		Share:    expandHome(get("SMB_SHARE_PATH")),
		Password: get("BACKUP_PASSWORD"),
		Compress: get("SSMBAK_COMPRESSION"),
		KeepDays: 30,
		KeepMin:  3,
	}
	if c.Compress == "" {
		c.Compress = "fast"
	}
	if pf := get("SSMBAK_PASSWORD_FILE"); c.Password == "" && pf != "" {
		b, err := os.ReadFile(expandHome(pf))
		if err != nil {
			return nil, fmt.Errorf("SSMBAK_PASSWORD_FILE: %w", err)
		}
		c.Password = strings.TrimRight(string(b), "\r\n")
	}
	if src := get("SSMBAK_SOURCE"); src != "" {
		for _, s := range filepath.SplitList(src) {
			if s = strings.TrimSpace(s); s != "" {
				c.Sources = append(c.Sources, expandHome(s))
			}
		}
	}
	if ex := get("SSMBAK_EXCLUDE"); ex != "" {
		c.Excludes = strings.Split(ex, ",")
	}
	for key, dst := range map[string]*int{"SSMBAK_RETENTION_DAYS": &c.KeepDays, "SSMBAK_KEEP_MIN": &c.KeepMin} {
		if v := get(key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%s must be a whole number >= 0, got %q", key, v)
			}
			*dst = n
		}
	}
	return c, nil
}
