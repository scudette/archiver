package main

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var (
	compress = app.Command("compress", "Compress a directory")
	size     = compress.Flag("file_size", "limit in MB of archive size (default 1024 = 1Gb per file)").
			Default("1024").Int()
	output = compress.Arg("output", "Name of the zip file to output").
		Required().String()
	files = compress.Arg("files", "Files to compress").
		Required().Strings()
)

type countWriter struct {
	count int
	fd    *os.File
}

func (self *countWriter) Write(buf []byte) (int, error) {
	n, err := self.fd.Write(buf)
	self.count += n
	return n, err
}

func (self *countWriter) Close() {
	self.fd.Close()
}

type Compressor struct {
	part       int
	size_limit int
	filename   string

	fd           *countWriter
	current_size int
	writer       *zip.Writer
}

func (self *Compressor) shouldCompress(path string) bool {
	extension := strings.ToLower(filepath.Ext(path))
	switch extension {
	case ".zip", ".jpg", ".png", ".rar", ".mp4", ".avi", ".mov":
		return false
	default:
		return true
	}
}

func (self *Compressor) AddFile(path string) error {
	fmt.Printf("Listing path %v\n", path)
	if self.fd.count > self.size_limit {
		err := self.PrepareVolume()
		if err != nil {
			return err
		}
	}

	fd, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fd.Close()

	stat, err := fd.Stat()
	if err != nil {
		return err
	}

	out_path := strings.TrimLeft(path, "/")
	method := zip.Store
	if self.shouldCompress(path) {
		method = zip.Deflate
	}

	w, err := self.writer.CreateHeader(&zip.FileHeader{
		Name:               out_path,
		Method:             method,
		Modified:           stat.ModTime(),
		UncompressedSize64: uint64(stat.Size()),
	})
	if err != nil {
		return err
	}
	_, err = io.Copy(w, fd)
	return err
}

func (self *Compressor) PrepareVolume() error {
	extension := filepath.Ext(self.filename)
	basename := strings.TrimSuffix(self.filename, extension)

	filename := fmt.Sprintf("%s-%04d%s", basename, self.part, extension)
	self.part++

	if self.writer != nil {
		self.writer.Close()
		self.fd.Close()
	}

	fmt.Printf("Creating new zip file %v\n", filename)
	out_fd, err := os.OpenFile(filename,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}

	if self.fd != nil {
		self.fd.Close()
	}

	self.fd = &countWriter{fd: out_fd}
	self.writer = zip.NewWriter(self.fd)

	return nil
}

func (self *Compressor) Close() (err error) {
	if self.writer != nil {
		err = self.writer.Close()
		self.writer = nil
	}

	if self.fd != nil {
		self.fd.Close()
		self.fd = nil
	}

	return err
}

func NewCompressor(size_limit int, filename string) *Compressor {
	res := &Compressor{
		size_limit: size_limit,
		filename:   filename,
	}
	res.PrepareVolume()
	return res
}

func doCompress() {
	compressor := NewCompressor(*size*1024*1024, *output)
	defer compressor.Close()

	for _, file := range *files {
		stat, err := os.Lstat(file)
		if err != nil {
			fmt.Printf("Error opening %v: %v\n", file, err)
			continue
		}

		if stat.IsDir() {
			err := filepath.WalkDir(file,
				func(path string, d fs.DirEntry, err error) error {
					if d.IsDir() {
						return nil
					}

					err = compressor.AddFile(path)
					if err != nil {
						fmt.Printf("Error opening %v: %v\n", path, err)
					}
					return nil
				})
			if err != nil {
				fmt.Printf("Error opening %v: %v\n", file, err)
				continue
			}
		}
	}
}

func init() {
	command_handlers = append(command_handlers, func(command string) bool {
		switch command {
		case compress.FullCommand():
			doCompress()
		default:
			return false
		}
		return true
	})
}
