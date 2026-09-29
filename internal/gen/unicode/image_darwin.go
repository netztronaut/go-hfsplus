//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// image is a bare (layout NONE) HFS+ disk image made and mounted with hdiutil.
type image struct {
	path string
	mnt  string
}

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// newImage creates work/name.dmg with file system fsType ("HFS+", "HFSX", ...)
// and mounts it read-write at work/name.mnt.
func newImage(work, name, fsType string, sizeMB int) (*image, error) {
	im := &image{path: filepath.Join(work, name+".dmg"), mnt: filepath.Join(work, name+".mnt")}
	os.Remove(im.path)
	if _, err := run("hdiutil", "create", "-quiet", "-size", fmt.Sprintf("%dm", sizeMB), "-fs", fsType,
		"-volname", "U", "-layout", "NONE", "-o", im.path); err != nil {
		return nil, err
	}
	return im, im.attach(false)
}

func (im *image) attach(readonly bool) error {
	if err := os.MkdirAll(im.mnt, 0o755); err != nil {
		return err
	}
	args := []string{"attach", "-quiet", "-nobrowse", "-noverify", "-noautofsck", "-mountpoint", im.mnt}
	if readonly {
		args = append(args, "-readonly")
	}
	_, err := run("hdiutil", append(args, im.path)...)
	return err
}

func (im *image) detach() error {
	_, err := run("hdiutil", "detach", "-quiet", im.mnt)
	if err != nil {
		_, err = run("hdiutil", "detach", "-quiet", "-force", im.mnt)
	}
	return err
}

func (im *image) remove() { os.Remove(im.path); os.Remove(im.mnt) }

// mkdir makes a directory in the image and returns its CNID.
func mkdir(path string) (uint32, error) {
	if err := os.Mkdir(path, 0o755); err != nil {
		return 0, err
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	return uint32(st.Ino), nil
}

// createResult is the outcome of an O_CREAT|O_EXCL create.
type createResult struct {
	Ino      uint32        // CNID of the new file, or of the existing one on EEXIST
	Errno    syscall.Errno // 0, EEXIST, or another error
	Existing bool          // EEXIST: Ino is the file the name collided with
}

// createExcl creates path exclusively and reports the file's CNID.
func createExcl(path string) createResult {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_EXCL|syscall.O_WRONLY|syscall.O_CLOEXEC, 0o644)
	if err != nil {
		var en syscall.Errno
		if !errors.As(err, &en) {
			en = syscall.EINVAL
		}
		r := createResult{Errno: en}
		if en == syscall.EEXIST {
			var st syscall.Stat_t
			if syscall.Stat(path, &st) == nil {
				r.Ino, r.Existing = uint32(st.Ino), true
			}
		}
		return r
	}
	var st syscall.Stat_t
	err = syscall.Fstat(fd, &st)
	syscall.Close(fd)
	if err != nil {
		return createResult{Errno: syscall.EIO}
	}
	return createResult{Ino: uint32(st.Ino)}
}

// createAll creates each name in dir and returns the results in order.
func createAll(dir string, names []string) []createResult {
	out := make([]createResult, len(names))
	for i, n := range names {
		out[i] = createExcl(dir + "/" + n)
	}
	return out
}

// storedByCNID maps the CNID of each child of dirID to its stored name.
func storedByCNID(c *catalog, dirID uint32) map[uint32][]uint16 {
	m := map[uint32][]uint16{}
	for _, r := range c.children(dirID) {
		m[r.CNID] = r.Name
	}
	return m
}

// batch creates names in a fresh directory of a fresh image, detaches it and
// returns, per name, the create result and the stored name, plus the on-disk
// children of the directory in key order.
type batchResult struct {
	Results []createResult
	Stored  [][]uint16 // stored name of the created (or collided-with) file; nil on other errors
	Order   []catRecord
	KCT     uint8
}

func runBatch(work, name, fsType string, sizeMB int, names []string) (*batchResult, error) {
	im, err := newImage(work, name, fsType, sizeMB)
	if err != nil {
		return nil, err
	}
	defer im.remove()
	dirID, err := mkdir(im.mnt + "/d")
	if err != nil {
		im.detach()
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "%s: creating %d names\n", name, len(names))
	res := createAll(im.mnt+"/d", names)
	if err := im.detach(); err != nil {
		return nil, err
	}
	c, err := readCatalog(im.path)
	if err != nil {
		return nil, err
	}
	st := storedByCNID(c, dirID)
	br := &batchResult{Results: res, Stored: make([][]uint16, len(names)), Order: c.children(dirID), KCT: c.KeyCompareType}
	for i, r := range res {
		if r.Ino != 0 {
			s, ok := st[r.Ino]
			if !ok {
				return nil, fmt.Errorf("%s: CNID %d of %q not in catalog", name, r.Ino, names[i])
			}
			br.Stored[i] = s
		}
	}
	return br, nil
}
