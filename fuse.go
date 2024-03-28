package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Velocidex/ttlcache/v2"
	kingpin "github.com/alecthomas/kingpin/v2"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

var (
	fuse_cmd       = app.Command("fuse", "Mount a set of Zip files as a fuse mount")
	fuse_directory = fuse_cmd.Arg("directory", "A directory to mount on").
			Required().String()

	fuse_tmp_dir = fuse_cmd.Flag("tmpdir",
		"A temporary directory to use (if not specified we use our own tempdir)").
		String()

	fuse_files = fuse_cmd.Arg("files", "list of zip files to mount").
			Required().Strings()
)

type ReaderAtCloser interface {
	io.ReaderAt
	io.Closer
}

// Manage temporary files
type tmpManager struct {
	mu      sync.Mutex
	tempdir string

	// Map between the zip filenames and the tempdir names
	files map[string]string

	// A Cache of file handles to avoid opening them too often.
	ttl *ttlcache.Cache
}

func (self *tmpManager) Close() {
	fmt.Printf("Clearing temp directory %v\n", self.tempdir)
	os.RemoveAll(self.tempdir)
}

func (self *tmpManager) Open(file *zip.File) (ReaderAtCloser, error) {
	self.mu.Lock()
	defer self.mu.Unlock()

	// Is it in the cache?
	cached_any, err := self.ttl.Get(file.Name)
	if err == nil {
		cached, ok := cached_any.(*cachedReaderAt)
		if ok {
			cached.IncRef()
			return cached, nil
		}
	}

	// No it is not in cache, maybe it is in the temp dir
	tmpfile, pres := self.files[file.Name]
	if !pres {
		h := sha256.New()
		h.Write([]byte(file.Name))
		tmpfile = filepath.Join(self.tempdir, hex.EncodeToString(h.Sum(nil)))
		out_fd, err := os.OpenFile(tmpfile,
			os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			return nil, err
		}
		defer out_fd.Close()

		fd, err := file.Open()
		if err != nil {
			return nil, err
		}

		_, err = io.Copy(out_fd, fd)
		if err != nil {
			return nil, err
		}
		fd.Close()
		out_fd.Close()

		self.files[file.Name] = tmpfile
	}

	fd, err := os.Open(tmpfile)
	if err != nil {
		return nil, err
	}

	// Cache for next time
	cached := &cachedReaderAt{
		ReaderAtCloser: fd,

		// One reference given to our caller
		refs: 1,
		name: file.Name,
	}
	self.ttl.Set(file.Name, cached)
	return cached, nil
}

type cachedReaderAt struct {
	ReaderAtCloser

	// Reference counting
	mu   sync.Mutex
	name string
	refs int
}

func (self *cachedReaderAt) IncRef() {
	self.mu.Lock()
	defer self.mu.Unlock()

	self.refs++
}

func (self *cachedReaderAt) Close() error {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.refs--

	return nil
}

func NewTmpManager(base string) (*tmpManager, error) {
	result := &tmpManager{
		files: make(map[string]string),
		ttl:   ttlcache.NewCache(),
	}

	dir, err := ioutil.TempDir(base, "prefix")
	if err != nil {
		return nil, fmt.Errorf("NewTmpManager: %w", err)
	}
	result.tempdir = dir

	fmt.Printf("INFO: Using tmpdir %v\n", result.tempdir)

	result.ttl.SetCheckExpirationCallback(
		func(key string, value interface{}) bool {
			cached, ok := value.(*cachedReaderAt)
			if !ok {
				return true
			}

			// Only allow it to expire where there are no references.
			cached.mu.Lock()
			defer cached.mu.Unlock()

			fmt.Printf("Refcount of fd for %v %v\n", key, cached.refs)
			if cached.refs == 0 {
				return true
			}
			return false
		})

	result.ttl.SetExpirationReasonCallback(
		func(key string, reason ttlcache.EvictionReason, value interface{}) {
			cached, ok := value.(*cachedReaderAt)
			if ok {
				cached.ReaderAtCloser.Close()
			}
		})

	// Keep 10 open files around
	result.ttl.SetCacheSizeLimit(10)

	return result, nil
}

// Build the directory tree
type FileNode struct {
	fs.Inode

	fd *zip.File

	mu   sync.Mutex
	data []byte

	tmp_manager *tmpManager
}

func (self FileNode) Debug(indent, name string) {
	fmt.Printf("%sFile: %s %v\n", indent, name, self.fd.UncompressedSize64)
}

func (self FileNode) Getattr(ctx context.Context,
	f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {

	out.Mode = uint32(0644)
	out.Nlink = 1
	out.Mtime = uint64(self.fd.Modified.Unix())
	out.Atime = out.Mtime
	out.Ctime = out.Mtime
	out.Size = self.fd.UncompressedSize64

	const bs = 512
	out.Blksize = bs
	out.Blocks = (out.Size + bs - 1) / bs

	return 0
}

func (self *FileNode) Open(
	ctx context.Context, flags uint32) (
	fs.FileHandle, uint32, syscall.Errno) {

	// We don't return a filehandle since we don't really need
	// one.  The file content is immutable, so hint the kernel to
	// cache the data.
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (self *FileNode) Read(ctx context.Context,
	f fs.FileHandle, dest []byte, off int64) (
	fuse.ReadResult, syscall.Errno) {

	fd, err := self.tmp_manager.Open(self.fd)
	if err != nil {
		fmt.Printf("ERROR: While opening %v: %v\n", self.fd.Name, err)
		return nil, syscall.EIO
	}
	defer fd.Close()

	n, err := fd.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		fmt.Printf("ERROR: While opening %v: %v\n", self.fd.Name, err)
		return nil, syscall.EIO
	}

	return fuse.ReadResultData(dest[:n]), 0
}

type DirectoryNode map[string]*Node

func (self DirectoryNode) Debug(indent, name string) {
	fmt.Printf("%sDirectory: %s\n", indent, name)

	keys := make([]string, 0, len(self))
	for k := range self {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		v, _ := self[k]
		v.Debug(indent+"  ", k)
	}
}

type Node struct {
	File      *FileNode
	Directory DirectoryNode

	tmp_manager *tmpManager
}

func (self *Node) Debug(indent, name string) {
	if self.File != nil {
		self.File.Debug(indent, name)
	} else {
		self.Directory.Debug(indent, name)
	}
}

func (self *Node) add(components []string, fd *zip.File) {

	if len(components) == 0 {
		self.File = &FileNode{
			fd:          fd,
			tmp_manager: self.tmp_manager,
		}
		return
	}

	dirname := components[0]
	if self.Directory == nil {
		self.Directory = make(DirectoryNode)
	}

	sub_node, pres := self.Directory[dirname]
	if !pres {
		sub_node = &Node{
			tmp_manager: self.tmp_manager,
		}
		self.Directory[dirname] = sub_node
	}

	sub_node.add(components[1:], fd)
}

func (self *Node) OnAdd(ctx context.Context, name string, node *fs.Inode) {
	keys := make([]string, 0, len(self.Directory))
	for k := range self.Directory {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		subnode, _ := self.Directory[k]

		// The subnode is a file
		if subnode.File != nil {
			child := node.NewPersistentInode(
				ctx, subnode.File, fs.StableAttr{})
			node.AddChild(k, child, true)
			continue
		}

		// Subnode is a directory
		child := node.NewPersistentInode(ctx, &fs.Inode{},
			fs.StableAttr{Mode: fuse.S_IFDIR})

		node.AddChild(k, child, true)
		subnode.OnAdd(ctx, k, child)
	}
}

type zipFS struct {
	fs.Inode

	fds         []*os.File
	zip_readers []*zip.Reader

	tree        *Node
	tmp_manager *tmpManager
}

func (self *zipFS) Close() {
	for _, f := range self.fds {
		f.Close()
	}
	self.tmp_manager.Close()
}

func (self *zipFS) OnAdd(ctx context.Context) {
	self.tree.OnAdd(ctx, "", &self.Inode)
}

func (self *zipFS) SplitPath(path string) (res []string) {
	path = strings.ReplaceAll(path, "\\", "/")
	components := strings.Split(path, "/")
	for _, c := range components {
		if c == "" || c == "." || c == ".." {
			continue
		}

		res = append(res, c)
	}

	return res
}

func NewZipFS(files []string) (*zipFS, error) {
	tmp_manager, err := NewTmpManager(*fuse_tmp_dir)
	if err != nil {
		return nil, err
	}

	fs := &zipFS{
		tree: &Node{
			tmp_manager: tmp_manager,
		},
		tmp_manager: tmp_manager,
	}

	for _, f := range files {
		fd, err := os.Open(f)
		if err != nil {
			fs.Close()
			return nil, err
		}
		fs.fds = append(fs.fds, fd)

		stat, err := fd.Stat()
		if err != nil {
			fs.Close()
			return nil, err
		}

		zip_reader, err := zip.NewReader(fd, stat.Size())
		if err != nil {
			fs.Close()
			return nil, err
		}

		for _, file := range zip_reader.File {
			fs.tree.add(fs.SplitPath(file.Name), file)
		}
	}

	return fs, nil
}

var _ = (fs.NodeOpener)((*FileNode)(nil))
var _ = (fs.NodeGetattrer)((*FileNode)(nil))
var _ = (fs.NodeOnAdder)((*zipFS)(nil))

func doFuse() {
	ctx, cancel := install_sig_handler()
	defer cancel()

	zip_fs, err := NewZipFS(*fuse_files)
	kingpin.FatalIfError(err, "Opening zip files")

	defer zip_fs.Close()

	ttl := time.Duration(5 * time.Second)

	opts := &fs.Options{
		AttrTimeout:  &ttl,
		EntryTimeout: &ttl,
	}

	server, err := fs.Mount(*fuse_directory, zip_fs, opts)
	kingpin.FatalIfError(err, "Mounting fuse")

	go func() {
		server.Wait()
	}()

	<-ctx.Done()
}

func init() {
	command_handlers = append(command_handlers, func(command string) bool {
		switch command {
		case fuse_cmd.FullCommand():
			doFuse()
		default:
			return false
		}
		return true
	})
}
