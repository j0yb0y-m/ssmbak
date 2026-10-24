package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"os"
)

// encryptFile encrypts a file using AES-GCM and prepends Salt (16B) and Nonce (12B)
func encryptFile(filePath, password string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}

	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", err
	}

	nonce := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	// Derive 32-byte key via PBKDF2-HMAC-SHA256
	key, err := pbkdf2.Key(sha256.New, password, salt, 100000, 32)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	ciphertext := aesgcm.Encrypt(nil, nonce, data, nil)

	outPath := filePath + ".enc"
	outFile, err := os.Create(outPath)
	if err != nil {
		return "", err
	}
	defer outFile.Close()

	// Write metadata header: Salt + Nonce + Ciphertext
	if _, err := outFile.Write(salt); err != nil {
		return "", err
	}
	if _, err := outFile.Write(nonce); err != nil {
		return "", err
	}
	if _, err := outFile.Write(ciphertext); err != nil {
		return "", err
	}

	return outPath, nil
}
