package hfsdiskfs

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"testing"

	"github.com/diskfs/go-diskfs/filesystem"

	"netztronaut.de/go-hfsplus/apm"
	"netztronaut.de/go-hfsplus/hfsplus"
)

func TestAdapter(t *testing.T) {
	f, err := os.Open("../testdata/images/jhfsplus-apm.img.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	disk, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(disk)
	table, err := ReadTable(r, int64(len(disk)))
	if err != nil {
		t.Fatal(err)
	}
	var hfs *Partition
	for _, p := range table.GetPartitions() {
		if pp := p.(*Partition); pp.Type == apm.TypeHFS {
			hfs = pp
		}
	}
	if hfs == nil {
		t.Fatal("no Apple_HFS partition")
	}
	fsys, err := OpenFileSystem(io.NewSectionReader(r, hfs.GetStart(), hfs.GetSize()), hfs.GetSize(), hfsplus.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if fsys.Type() != TypeHFSPlus || fsys.Label() != "jhfsplus-apm" {
		t.Errorf("type %v label %q", fsys.Type(), fsys.Label())
	}
	b, err := fs.ReadFile(fsys, "System/Library/CoreServices/SystemVersion.plist")
	if err != nil || !bytes.Contains(b, []byte("10.4.11")) {
		t.Errorf("SystemVersion.plist: %v", err)
	}
	if err := fsys.Mkdir("x"); !errors.Is(err, filesystem.ErrReadonlyFilesystem) {
		t.Errorf("Mkdir: %v", err)
	}
	if _, err := fsys.OpenFile("a/b/file.txt", os.O_RDWR); !errors.Is(err, filesystem.ErrReadonlyFilesystem) {
		t.Errorf("OpenFile for writing: %v", err)
	}
	h, err := fsys.OpenFile("a/b/file.txt", os.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Write([]byte("x")); !errors.Is(err, filesystem.ErrReadonlyFilesystem) {
		t.Errorf("Write: %v", err)
	}
	if err := table.Write(nil, 0); !errors.Is(err, filesystem.ErrReadonlyFilesystem) {
		t.Errorf("Table.Write: %v", err)
	}
}
