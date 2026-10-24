package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

const (
	BackupNamePrefix = "ssmbak_backup"
	RetentionDays    = 30
)

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func main() {
	fmt.Println("Secure SMB Backup")
	// setup logging
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	// determine path based on OS
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Failed to determine home directory: %v", err)
	}

	// source folder to backup
	source := getEnv("SSMBAK_SOURCE", homeDir)
	sourceFolder := fmt.Sprintf("%s/%s", source, BackupNamePrefix)
	fmt.Printf("Source Folder: %s\n", sourceFolder)

	// smb share path
	smbSharePath := getEnv("SMB_SHARE_PATH", "")
	if smbSharePath == "" {
		log.Fatal("SMB_SHARE_PATH is not set")
	}
	fmt.Printf("Source: %s\n", source)

	// smb share point
	smbShare := getEnv("SMB_SHARE_PATH", "")
	if smbShare == "" {
		log.Fatal("SMB_SHARE_PATH is not set")
	}
	fmt.Printf("SMB Share: %s\n", smbShare)

	// backup password
	password := getEnv("BACKUP_PASSWORD", "")
	if password == "" {
		log.Fatal("BACKUP_PASSWORD is not set")
	}
	fmt.Printf("Backup Password: %s\n", password)

	if _, err := os.Stat(sourceFolder); os.IsNotExist(err) {
		log.Fatalf("Source folder does not exist: %s", sourceFolder)
	}
	if _, err := os.Stat(smbSharePath); os.IsNotExist(err) {
		log.Fatalf("SMB share path is not accessible or mounted: %s", smbSharePath)
	}

	timestamp := time.Now().Format("20060102_150405")
	archiveBaseName := BackupNamePrefix + "_" + timestamp

	tempDir := os.TempDir()
	zipFile := filepath.Join(tempDir, archiveBaseName+".zip")

	log.Printf("Starting compression of %s...", sourceFolder)
	if err := createZip(sourceFolder, zipFile); err != nil {
		log.Fatalf("Compression failed: %v", err)
	}
	log.Printf("Compression complete: %s", zipFile)

	// Ensure local temp zip cleanup on exit
	defer os.Remove(zipFile)

	// Client-Side Encryption (AES-GCM)
	log.Printf("Encrypting archive...")
	encryptedFile, err := encryptFile(zipFile, password)
	if err != nil {
		log.Fatalf("Encryption failed: %v", err)
	}
	defer os.Remove(encryptedFile)
	log.Printf("Encryption complete: %s", filepath.Base(encryptedFile))

	// Transfer to SMB Share
	destinationPath := filepath.Join(smbSharePath, filepath.Base(encryptedFile))
	log.Printf("Copying encrypted backup to SMB share: %s", destinationPath)
	if err := copyFile(encryptedFile, destinationPath); err != nil {
		log.Fatalf("Failed to copy file to SMB share: %v", err)
	}
	log.Println("Secure backup successfully transferred to SMB share.")

	// Cleanup old backups
	cleanupOldBackups(smbSharePath, BackupNamePrefix, RetentionDays)

}
