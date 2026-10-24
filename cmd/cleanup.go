package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cleanupOldBackups deletes backups older than the retention threshold
func cleanupOldBackups(dir string, prefix string, days int) {
	log.Printf("Checking for backups older than %d days on SMB share...", days)
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("Failed to read SMB directory: %v", err)
		return
	}

	now := time.Now()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".enc") {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if now.Sub(info.ModTime()).Hours() > float64(days*24) {
				fullPath := filepath.Join(dir, name)
				if err := os.Remove(fullPath); err != nil {
					log.Printf("Failed to delete old backup %s: %v", name, err)
				} else {
					log.Printf("Deleted old backup: %s", name)
				}
			}
		}
	}
}
