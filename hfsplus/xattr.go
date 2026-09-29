package hfsplus

import (
	"errors"
	"io"
	"strings"
)

// xattrSystemPrefix starts the attribute names xnu protects and listxattr(2) does not list.
const xattrSystemPrefix = "com.apple.system."

// ErrNoXattr is returned, in an *fs.PathError, for an extended attribute the file does not have.
var ErrNoXattr = errors.New("attribute not found")

// maxXattrSize bounds the attribute value GetXattr returns; macOS limits HFS+ attributes to
// 128 KiB except the resource fork, which OpenResourceFork streams.
const maxXattrSize = 128 << 10

// Attribute is an extended attribute as the attributes B-tree stores it.
type Attribute struct {
	Name   string
	Size   int64
	Inline bool // held in the B-tree record rather than in extents of its own
}

// Attributes returns the attributes the attributes B-tree holds for the named file, without
// following a final symbolic link: what is on disk, including com.apple.decmpfs, and without the
// Finder information and resource fork macOS presents as attributes.
func (v *Volume) Attributes(name string) ([]Attribute, error) {
	r, err := v.resolve("listxattr", name, false)
	if err != nil {
		return nil, err
	}
	as, err := v.attrs(v.ctx, r.CNID)
	if err != nil {
		return nil, pathError("listxattr", name, err)
	}
	out := make([]Attribute, len(as))
	for i, a := range as {
		out[i] = Attribute{Name: a.name, Size: a.size(), Inline: a.fork == nil}
	}
	return out, nil
}

// finderInfoXattr returns the 32 bytes macOS presents as com.apple.FinderInfo, with the fields it
// hides zeroed, or nil when they are all zero, in which case macOS does not list it.
func finderInfoXattr(r *Record) []byte {
	b := make([]byte, 32)
	copy(b, r.UserInfo[:])
	copy(b[16:], r.FinderInfo[:])
	if r.isSymlink() {
		// A symbolic link's 'slnk'/'rhap' type and creator are the implementation's.
		clear(b[0:8])
	}
	// document_id, date_added and write_gen_counter of the extended Finder information.
	clear(b[16:24])
	clear(b[28:32])
	for _, c := range b {
		if c != 0 {
			return b
		}
	}
	return nil
}

// ListXattr lists the named file's extended attributes as macOS's listxattr(2) does, without
// following a final symbolic link: com.apple.FinderInfo when the Finder information is not
// empty, com.apple.ResourceFork when the file has a resource fork and is not compressed, then the
// attributes B-tree's in its order, hiding com.apple.decmpfs of a compressed file and the
// com.apple.system. attributes xnu protects, such as the com.apple.system.hfs.firstlink of a
// directory hard link.
func (v *Volume) ListXattr(name string) ([]string, error) {
	r, err := v.resolve("listxattr", name, false)
	if err != nil {
		return nil, err
	}
	var out []string
	if finderInfoXattr(r) != nil {
		out = append(out, XattrFinderInfo)
	}
	if !r.IsFolder && r.ResourceFork.LogicalSize > 0 && !r.Compressed() {
		out = append(out, XattrResourceFork)
	}
	as, err := v.attrs(v.ctx, r.CNID)
	if err != nil {
		return nil, pathError("listxattr", name, err)
	}
	for _, a := range as {
		if a.name == XattrDecmpfs && r.Compressed() || a.name == XattrFinderInfo || a.name == XattrResourceFork ||
			strings.HasPrefix(a.name, xattrSystemPrefix) {
			continue
		}
		out = append(out, a.name)
	}
	return out, nil
}

// GetXattr returns the value of the named file's extended attribute attr, as macOS's
// getxattr(2) does, without following a final symbolic link. A resource fork larger than 128 KiB
// is refused; OpenResourceFork streams it.
func (v *Volume) GetXattr(name, attr string) ([]byte, error) {
	r, err := v.resolve("getxattr", name, false)
	if err != nil {
		return nil, err
	}
	switch attr {
	case XattrFinderInfo:
		if b := finderInfoXattr(r); b != nil {
			return b, nil
		}
		return nil, pathError("getxattr", name, ErrNoXattr)
	case XattrResourceFork:
		if r.IsFolder || r.ResourceFork.LogicalSize == 0 || r.Compressed() {
			return nil, pathError("getxattr", name, ErrNoXattr)
		}
		if r.ResourceFork.LogicalSize > maxXattrSize {
			return nil, pathError("getxattr", name, unsupported("a %d-byte resource fork as an attribute", r.ResourceFork.LogicalSize))
		}
		f, err := v.newFork("resource fork", r.ResourceFork, v.overflow(r.CNID, forkResource))
		if err != nil {
			return nil, pathError("getxattr", name, err)
		}
		b := make([]byte, f.size)
		if _, err := f.readAt(v.ctx, b, 0); err != nil && err != io.EOF {
			return nil, pathError("getxattr", name, err)
		}
		return b, nil
	case XattrDecmpfs:
		if r.Compressed() {
			return nil, pathError("getxattr", name, ErrNoXattr)
		}
	}
	a, err := v.attr(v.ctx, r.CNID, attr)
	if err != nil {
		if errors.Is(err, errInvalidName) {
			return nil, pathError("getxattr", name, ErrNoXattr)
		}
		return nil, pathError("getxattr", name, err)
	}
	if a == nil {
		return nil, pathError("getxattr", name, ErrNoXattr)
	}
	b, err := v.readAttr(v.ctx, r.CNID, a, maxXattrSize)
	if err != nil {
		return nil, pathError("getxattr", name, err)
	}
	return b, nil
}

// Record returns the catalog record of the named file without following a final symbolic link;
// a hard link resolves to its target, with the link's own record in Link.
func (v *Volume) Record(name string) (*Record, error) {
	return v.resolve("stat", name, false)
}
