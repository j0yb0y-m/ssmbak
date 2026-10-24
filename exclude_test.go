package main

import "testing"

func TestDefaultExcludes(t *testing.T) {
	m, err := newMatcher(defaultExcludes)
	if err != nil {
		t.Fatal(err)
	}
	excluded := []string{
		"node_modules", "app/node_modules", "a/b/c/node_modules",
		".venv", "projects/api/.venv", "venv", "x/venv",
		"x/__pycache__", "x/mod.pyc", "x/lib/python3.12/site-packages",
		".cache", "a/.cache", ".npm", ".cargo/registry", ".rustup",
		"go/pkg/mod", ".local/share/Trash", ".config/Code/Cache", ".config/Code/GPUCache",
		".var/app/org.foo.Bar/cache", ".config/ssmbak", "Pictures/.DS_Store", "docs/Thumbs.db", "notes.txt~", ".Trash-1000",
	}
	for _, p := range excluded {
		if rule, ok := m.match(p); !ok {
			t.Errorf("%q should be excluded", p)
		} else if rule == "" {
			t.Errorf("%q matched with an empty rule", p)
		}
	}
	kept := []string{
		"Documents", "Documents/thesis.docx", "Pictures/holiday.jpg", "projects/api/main.go",
		"projects/api/.git", "projects/api/.env", ".ssh/id_ed25519", ".bashrc", ".config/nvim/init.lua",
		"notes/node_modules_explained.md", "venv_setup.md", "my.venv", "go/src/app/main.go",
		"go/pkg", ".cargo/bin", ".config/Code/User/settings.json", "cache", "mod",
	}
	for _, p := range kept {
		if rule, ok := m.match(p); ok {
			t.Errorf("%q must NOT be excluded (matched %q)", p, rule)
		}
	}
}

func TestCustomExcludes(t *testing.T) {
	m, err := newMatcher(defaultExcludes, []string{"target", " *.iso ", "Downloads/big/", "./build", ""})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"rust/proj/target", "a/b.iso", "Downloads/big", "build"} {
		if _, ok := m.match(p); !ok {
			t.Errorf("%q should be excluded", p)
		}
	}
	if _, ok := m.match("other/Downloads/big"); ok {
		t.Error("a path pattern must be anchored to the source folder")
	}
}

func TestBadExcludePattern(t *testing.T) {
	if _, err := newMatcher([]string{"[abc"}); err == nil {
		t.Fatal("expected an error for a malformed pattern")
	}
}
