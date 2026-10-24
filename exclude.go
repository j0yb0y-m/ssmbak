package main

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// defaultExcludes is the built-in "junk" list. Patterns work like this:
//
//   - no "/" in the pattern: matches a file or folder NAME at any depth
//     ("node_modules" skips every node_modules folder, wherever it is)
//   - with a "/": matches a path relative to the folder being backed up
//     ("go/pkg/mod" skips ~/go/pkg/mod when your home folder is the source)
//   - * ? [a-z] wildcards work, but * never matches "/"
//
// Add your own with --exclude or SSMBAK_EXCLUDE. Run `ssmbak backup --dry-run`
// to see exactly what gets skipped.
var defaultExcludes = []string{
	// Language dependencies and virtual environments
	"node_modules", "bower_components",
	".venv", "venv", "site-packages", "__pycache__", "*.pyc", "*.egg-info", ".eggs", ".tox", ".nox",
	".pytest_cache", ".mypy_cache", ".ruff_cache",
	".gradle", ".terraform",
	// Framework build caches
	".next", ".nuxt", ".parcel-cache", ".turbo",

	// Package-manager and toolchain folders in your home folder
	".npm", ".nvm", ".pyenv", ".rustup", ".sdkman",
	".cargo/registry", ".cargo/git", ".m2/repository", ".nuget/packages", "go/pkg/mod",

	// Caches, trash, virtual filesystems
	".cache", ".local/share/Trash", ".Trash", ".Trash-*", ".gvfs",
	".config/*/Cache", ".config/*/Code Cache", ".config/*/GPUCache", ".config/*/CachedData",
	".var/app/*/cache",

	// ssmbak's own config holds the backup password; never store the key next to the lock
	".config/ssmbak",

	// OS and editor leftovers
	".DS_Store", "Thumbs.db", "desktop.ini", "*.swp", "*.tmp", "*~",
}

type matcher struct {
	nameLit map[string]string // literal base names -> rule
	pathLit map[string]string // literal relative paths -> rule
	globs   []string
}

func newMatcher(lists ...[]string) (*matcher, error) {
	m := &matcher{nameLit: map[string]string{}, pathLit: map[string]string{}}
	for _, list := range lists {
		for _, raw := range list {
			p := filepath.ToSlash(strings.TrimSpace(raw)) // no-op on Linux/macOS
			p = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(p, "./"), "/"), "/")
			if p == "" {
				continue
			}
			if _, err := path.Match(p, "x"); err != nil {
				return nil, fmt.Errorf("bad exclude pattern %q: %w", raw, err)
			}
			switch {
			case strings.ContainsAny(p, `*?[\`):
				m.globs = append(m.globs, p)
			case strings.Contains(p, "/"):
				m.pathLit[p] = p
			default:
				m.nameLit[p] = p
			}
		}
	}
	return m, nil
}

// match reports whether rel (slash-separated, relative to the source folder)
// is excluded, and which rule matched.
func (m *matcher) match(rel string) (rule string, ok bool) {
	base := path.Base(rel)
	if r, ok := m.nameLit[base]; ok {
		return r, true
	}
	if r, ok := m.pathLit[rel]; ok {
		return r, true
	}
	for _, g := range m.globs {
		target := base
		if strings.Contains(g, "/") {
			target = rel
		}
		if ok, _ := path.Match(g, target); ok {
			return g, true
		}
	}
	return "", false
}
