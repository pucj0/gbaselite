package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	format := flag.String("format", "", "archive format: zip or tar.gz")
	source := flag.String("source", "", "directory whose contents are archived")
	output := flag.String("output", "", "archive output path")
	flag.Parse()
	if *source == "" || *output == "" {
		fatalf("-source and -output are required")
	}
	var err error
	switch *format {
	case "zip":
		err = writeZIP(*source, *output)
	case "tar.gz":
		err = writeTarGZ(*source, *output)
	default:
		err = fmt.Errorf("unsupported archive format %q", *format)
	}
	if err != nil {
		fatalf("%v", err)
	}
}

func archiveEntries(root string) ([]string, error) {
	var entries []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != root {
			entries = append(entries, path)
		}
		return nil
	})
	return entries, err
}

func writeZIP(source, output string) (resultErr error) {
	entries, err := archiveEntries(source)
	if err != nil {
		return err
	}
	file, err := os.Create(output)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); resultErr == nil {
			resultErr = closeErr
		}
	}()
	writer := zip.NewWriter(file)
	defer func() {
		if closeErr := writer.Close(); resultErr == nil {
			resultErr = closeErr
		}
	}()
	for _, path := range entries {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if info.IsDir() {
			name += "/"
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = name
		header.Method = zip.Deflate
		if isExecutable(name) {
			header.SetMode(0o755)
		}
		destination, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			if err := copyFile(destination, path); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeTarGZ(source, output string) (resultErr error) {
	entries, err := archiveEntries(source)
	if err != nil {
		return err
	}
	file, err := os.Create(output)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); resultErr == nil {
			resultErr = closeErr
		}
	}()
	gzipWriter := gzip.NewWriter(file)
	defer func() {
		if closeErr := gzipWriter.Close(); resultErr == nil {
			resultErr = closeErr
		}
	}()
	tarWriter := tar.NewWriter(gzipWriter)
	defer func() {
		if closeErr := tarWriter.Close(); resultErr == nil {
			resultErr = closeErr
		}
	}()
	for _, path := range entries {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = name
		if info.IsDir() || isExecutable(name) {
			header.Mode = 0o755
		} else {
			header.Mode = 0o644
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		if !info.IsDir() {
			if err := copyFile(tarWriter, path); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFile(destination io.Writer, path string) error {
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()
	_, err = io.Copy(destination, source)
	return err
}

func isExecutable(name string) bool {
	base := filepath.Base(name)
	return base == "gbaselite" || strings.HasSuffix(base, ".sh")
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "archive: "+format+"\n", args...)
	os.Exit(1)
}
