// Package hfsdiskfs adapts the read-only Apple Partition Map and HFS+ readers to go-diskfs's
// partition.Table and filesystem.FileSystem interfaces.
//
// It is a module of its own so that the readers carry no dependency on go-diskfs. go-diskfs's
// interfaces include write methods; here every one of them returns
// filesystem.ErrReadonlyFilesystem without touching the disk.
package hfsdiskfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/diskfs/go-diskfs/backend"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition"
	"github.com/diskfs/go-diskfs/partition/part"

	"github.com/netztronaut/go-hfsplus-reader/apm"
	"github.com/netztronaut/go-hfsplus-reader/hfsplus"
)

// TypeHFSPlus is the filesystem.Type the adapter reports; go-diskfs has no constant for HFS+.
const TypeHFSPlus filesystem.Type = 0x482B // "H+"

var errReadOnly = filesystem.ErrReadonlyFilesystem

// Table is an Apple Partition Map as a go-diskfs partition.Table.
type Table struct {
	Map *apm.Map
}

var _ partition.Table = (*Table)(nil)

// ReadTable reads the Apple Partition Map of f, which is size bytes long.
func ReadTable(f io.ReaderAt, size int64) (*Table, error) {
	m, err := apm.Read(f, size)
	if err != nil {
		return nil, err
	}
	return &Table{Map: m}, nil
}

func (t *Table) Type() string { return "apm" }
func (t *Table) UUID() string { return "" }

func (t *Table) Write(backend.WritableFile, int64) error { return errReadOnly }
func (t *Table) Repair(uint64) error                     { return errReadOnly }

// Verify reads the map from f again and checks it is the same.
func (t *Table) Verify(f backend.File, diskSize uint64) error {
	m, err := apm.Read(f, int64(diskSize))
	if err != nil {
		return err
	}
	if len(m.Partitions) != len(t.Map.Partitions) {
		return fmt.Errorf("hfsdiskfs: map has %d entries, table %d", len(m.Partitions), len(t.Map.Partitions))
	}
	for i, p := range m.Partitions {
		if p.Raw != t.Map.Partitions[i].Raw {
			return fmt.Errorf("hfsdiskfs: entry %d differs", p.Index)
		}
	}
	return nil
}

func (t *Table) GetPartitions() []part.Partition {
	ps := make([]part.Partition, len(t.Map.Partitions))
	for i := range t.Map.Partitions {
		ps[i] = &Partition{Partition: &t.Map.Partitions[i]}
	}
	return ps
}

// Partition is one partition map entry as a go-diskfs part.Partition.
type Partition struct {
	*apm.Partition
}

func (p *Partition) GetIndex() int  { return p.Index }
func (p *Partition) GetSize() int64 { return p.Length }
func (p *Partition) GetStart() int64 {
	return p.Start
}
func (p *Partition) UUID() string  { return "" }
func (p *Partition) Label() string { return p.Name }

// ReadContents copies the partition's bytes from f to out.
func (p *Partition) ReadContents(f backend.File, out io.Writer) (int64, error) {
	return io.Copy(out, io.NewSectionReader(f, p.Start, p.Length))
}

func (p *Partition) WriteContents(backend.WritableFile, io.Reader) (uint64, error) {
	return 0, errReadOnly
}

// FileSystem is an HFS+ volume as a go-diskfs filesystem.FileSystem.
type FileSystem struct {
	*hfsplus.Volume
}

var _ filesystem.FileSystem = (*FileSystem)(nil)

// OpenFileSystem opens the HFS+ volume in r, a partition of size bytes.
func OpenFileSystem(r io.ReaderAt, size int64, opts hfsplus.Options) (*FileSystem, error) {
	v, err := hfsplus.Open(r, size, opts)
	if err != nil {
		return nil, err
	}
	return &FileSystem{Volume: v}, nil
}

func (f *FileSystem) Type() filesystem.Type { return TypeHFSPlus }

// Label returns the volume name, or "" if it cannot be read.
func (f *FileSystem) Label() string {
	name, err := f.Volume.Name()
	if err != nil {
		return ""
	}
	return name
}

func (f *FileSystem) Close() error { return nil }

func (f *FileSystem) Mkdir(string) error                                    { return errReadOnly }
func (f *FileSystem) Mknod(string, uint32, int) error                       { return errReadOnly }
func (f *FileSystem) Link(string, string) error                             { return errReadOnly }
func (f *FileSystem) Symlink(string, string) error                          { return errReadOnly }
func (f *FileSystem) Chmod(string, os.FileMode) error                       { return errReadOnly }
func (f *FileSystem) Chown(string, int, int) error                          { return errReadOnly }
func (f *FileSystem) Chtimes(string, time.Time, time.Time, time.Time) error { return errReadOnly }
func (f *FileSystem) Rename(string, string) error                           { return errReadOnly }
func (f *FileSystem) Remove(string) error                                   { return errReadOnly }
func (f *FileSystem) SetLabel(string) error                                 { return errReadOnly }

// OpenFile opens a file for reading; any flag that asks for writing fails.
func (f *FileSystem) OpenFile(name string, flag int) (filesystem.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0 {
		return nil, &fs.PathError{Op: "open", Path: name, Err: errReadOnly}
	}
	h, err := f.Volume.Open(name)
	if err != nil {
		return nil, err
	}
	return &File{File: h}, nil
}

// File is an open file whose Write fails.
type File struct {
	fs.File
}

func (f *File) Read(p []byte) (int, error) { return f.File.Read(p) }

func (f *File) Seek(offset int64, whence int) (int64, error) {
	s, ok := f.File.(io.Seeker)
	if !ok {
		return 0, errors.New("hfsdiskfs: a directory cannot seek")
	}
	return s.Seek(offset, whence)
}

func (f *File) Write([]byte) (int, error) { return 0, errReadOnly }
