package main

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
)

// createZip compresses a directory into a zip archive using standard Deflate
func createZip(source, target string) error {
	zipFile, err := os.Create(target)
	if err != nil {
		return err
	}
	defer zipFile.Close()

	archive := zip.NewWriter(zipFile)
	defer archive.Close()

	baseDir := filepath.Dir(source)

	return filepath.WalkDir(source, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(baseDir, path)
		if err != nil {
			return err
		}

		if relPath == "." {
			return nil
		}

		if d.IsDir() {
			_, err = archive.Create(relPath + "/")
			return err
		}

		fileWriter, err := archive.Create(relPath)
		if err != nil {
			return err
		}

		fileReader, err := os.Open(path)
		if err != nil {
			return err
		}
		defer fileReader.Close()

		_, err = io.Copy(fileWriter, fileReader)
		return err
	})
}
