package hfsplus

import (
	"bytes"
	"encoding/binary"
	"io/fs"
	"testing"

	"netztronaut.de/go-hfsplus/apm"
)

// TestInstallCD reads a disc laid out as the Mac OS X 10.2 install CD (6C115) is, with a map at
// 512-byte strides and one at 2048-byte strides sharing two Apple_Void slots
// (testdata/installcd.py), from the partition map to SystemVersion.plist, as detection does.
func TestInstallCD(t *testing.T) {
	disk := loadImage(t, "installcd-apm")

	// The disc carries both maps, and walked at the other's stride neither agrees with itself.
	if binary.BigEndian.Uint32(disk[512+4:]) != 10 || binary.BigEndian.Uint32(disk[3*2048+4:]) != 9 {
		t.Fatal("the image does not carry a 10-entry map at 512 and a 9-entry one at 2048")
	}
	if binary.BigEndian.Uint32(disk[2048+4:]) != 10 || string(disk[2048+48:2048+58]) != apm.TypeVoid {
		t.Fatal("the 2048 map's first entry is not the 512 map's Apple_Void")
	}

	m, err := apm.Read(bytes.NewReader(disk), int64(len(disk)))
	if err != nil {
		t.Fatal(err)
	}
	if !m.HasDDR || m.BlockSize != 2048 || int64(m.BlockCount)*2048 != int64(len(disk)) {
		t.Errorf("DDR: block size %d, count %d", m.BlockSize, m.BlockCount)
	}
	if len(m.Drivers) != 4 {
		t.Errorf("%d driver descriptors", len(m.Drivers))
	}
	if m.EntrySize != 512 || m.Unit != 512 || len(m.Partitions) != 10 {
		t.Fatalf("entry size %d, unit %d, %d entries", m.EntrySize, m.Unit, len(m.Partitions))
	}
	for _, i := range []int{3, 7} {
		if p := m.Partitions[i]; p.Type != apm.TypeVoid || p.Length != 0 {
			t.Errorf("entry %d: %q, %d bytes", p.Index, p.Type, p.Length)
		}
	}
	p := m.Find(apm.TypeHFS)
	if p == nil || p.Index != 9 || p.Name != "Mac_OS_X" || p.Start != 968*512 || p.Processor != "powerpc" {
		t.Fatalf("Apple_HFS: %+v", p)
	}
	// The 2048 map's own Apple_HFS entry, in 2048-byte blocks, names the same bytes.
	e := disk[9*2048:]
	if string(e[48:57]) != apm.TypeHFS || int64(binary.BigEndian.Uint32(e[8:]))*2048 != p.Start ||
		int64(binary.BigEndian.Uint32(e[12:]))*2048 != p.Length {
		t.Errorf("the 2048 map's Apple_HFS entry does not match the 512 map's")
	}

	v, err := Open(p.Section(), p.Length, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if name, err := v.Name(); err != nil || name != "Mac_OS_X" {
		t.Errorf("Name = %q, %v", name, err)
	}
	// On 10.2 the System Folder is blessed and the boot file is not.
	b, ok, err := v.Blessed()
	if err != nil || !ok {
		t.Fatalf("Blessed = %+v, %v, %v", b, ok, err)
	}
	if b.SystemFolderPath != "System/Library/CoreServices" || b.BootFile != 0 || b.BootFilePath != "" {
		t.Errorf("blessed %+v", b)
	}
	if got, w := [6]uint32{b.SystemFolder, b.BootFile, b.OpenFolder, b.OS9Folder, 0, b.OSXFolder}, blessInfo(t, "installcd-apm"); got != w {
		t.Errorf("finderinfo %v, bless --info says %v", got, w)
	}
	plist, err := fs.ReadFile(v, "System/Library/CoreServices/SystemVersion.plist")
	if err != nil || !bytes.Contains(plist, []byte("<string>10.2</string>")) || !bytes.Contains(plist, []byte("<string>6C115</string>")) {
		t.Errorf("SystemVersion.plist: %v\n%s", err, plist)
	}
}
