package hfsplus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

// The Volume implements these; the go-diskfs adapter, which needs write methods, lives in its own
// module.
var (
	_ fs.FS         = (*Volume)(nil)
	_ fs.StatFS     = (*Volume)(nil)
	_ fs.ReadDirFS  = (*Volume)(nil)
	_ fs.ReadFileFS = (*Volume)(nil)
	_ fs.ReadLinkFS = (*Volume)(nil)
)

// BSD file types, the S_IFMT bits of BSDInfo.FileMode.
const (
	modeTypeMask = 0o170000
	modeFIFO     = 0o010000
	modeChar     = 0o020000
	modeDir      = 0o040000
	modeBlock    = 0o060000
	modeRegular  = 0o100000
	modeSymlink  = 0o120000
	modeSocket   = 0o140000
	modeWhiteout = 0o160000
)

// Finder types and creators of links.
var (
	typeSymlink     = fourCC("slnk")
	creatorSymlink  = fourCC("rhap")
	typeHardLink    = fourCC("hlnk")
	creatorHardLink = fourCC("hfs+")
	typeDirLink     = fourCC("fdrp")
	creatorDirLink  = fourCC("MACS")
)

// ErrNotDir is returned, in an *fs.PathError, when a path goes through a file as if it were a
// folder.
var ErrNotDir = errors.New("not a directory")

// maxSymlinkSize bounds a symbolic link's target; PATH_MAX is 1024.
const maxSymlinkSize = 4096

// bsdType returns a record's S_IFMT bits, derived from the record when the volume stored none,
// as volumes written by Mac OS 9 do.
func (r *Record) bsdType() uint16 {
	if t := r.BSD.FileMode & modeTypeMask; t != 0 {
		return t
	}
	switch {
	case r.IsFolder:
		return modeDir
	case r.Type() == typeSymlink && r.Creator() == creatorSymlink:
		return modeSymlink
	}
	return modeRegular
}

// Mode returns the record's permissions and type as an fs.FileMode.
func (r *Record) Mode() fs.FileMode {
	perm := r.BSD.FileMode & 0o777
	if r.BSD.FileMode&modeTypeMask == 0 {
		perm = 0o644
		if r.IsFolder {
			perm = 0o755
		}
	}
	m := fs.FileMode(perm)
	if r.BSD.FileMode&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if r.BSD.FileMode&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if r.BSD.FileMode&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	switch r.bsdType() {
	case modeDir:
		m |= fs.ModeDir
	case modeSymlink:
		m |= fs.ModeSymlink
	case modeFIFO:
		m |= fs.ModeNamedPipe
	case modeChar:
		m |= fs.ModeDevice | fs.ModeCharDevice
	case modeBlock:
		m |= fs.ModeDevice
	case modeSocket:
		m |= fs.ModeSocket
	case modeWhiteout:
		m |= fs.ModeIrregular
	}
	return m
}

func (r *Record) isSymlink() bool { return !r.IsFolder && r.bsdType() == modeSymlink }

// Compressed reports whether the file is decmpfs-compressed: flagged UF_COMPRESSED. Its data
// fork is then empty and its contents come from the com.apple.decmpfs attribute and the resource
// fork.
func (r *Record) Compressed() bool { return !r.IsFolder && r.BSD.OwnerFlags&flagUFCompressed != 0 }

// resolveHardLink returns the record a file or directory hard link points to, or r itself.
func (v *volume) resolveHardLink(ctx context.Context, r *Record) (*Record, error) {
	if r.IsFolder {
		return r, nil
	}
	var dir uint32
	var name []uint16
	switch {
	case r.Type() == typeHardLink && r.Creator() == creatorHardLink && v.privateDir != 0:
		dir, name = v.privateDir, cnidName("iNode", r.BSD.Special)
	case r.Type() == typeDirLink && r.Creator() == creatorDirLink && v.privateDirDir != 0:
		dir, name = v.privateDirDir, cnidName("dir_", r.BSD.Special)
	default:
		return r, nil
	}
	t, err := v.lookup(ctx, dir, name)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, corrupt("catalog B-tree", -1, "hard link %q (CNID %d) points to missing %s", r.Name, r.CNID, nameFromHFS(name))
	}
	t.Link = r
	t.Name = r.Name
	t.ParentID = r.ParentID
	return t, nil
}

// readLink reads a symbolic link's target from its data fork.
func (v *volume) readLink(ctx context.Context, r *Record) (string, error) {
	if r.DataFork.LogicalSize > maxSymlinkSize {
		return "", corrupt("catalog B-tree", -1, "symbolic link CNID %d is %d bytes", r.CNID, r.DataFork.LogicalSize)
	}
	rd, err := v.openData(ctx, r)
	if err != nil {
		return "", err
	}
	b := make([]byte, r.DataFork.LogicalSize)
	if _, err := rd.ReadAt(b, 0); err != nil && err != io.EOF {
		return "", err
	}
	return string(b), nil
}

// resolve returns the record name refers to, following symbolic links in every component but the
// last, and in the last too when follow is set. Links resolve within the volume: an absolute
// target starts at the volume root, and ".." at the root stays there.
func (v *Volume) resolve(op, name string, follow bool) (*Record, error) {
	if !fs.ValidPath(name) {
		return nil, pathError(op, name, fs.ErrInvalid)
	}
	ctx := v.ctx
	if err := ctx.Err(); err != nil {
		return nil, pathError(op, name, err)
	}
	root := *v.root
	stack := []*Record{&root}
	var parts []string
	if name != "." {
		parts = strings.Split(name, "/")
	}
	hops := 0
	for len(parts) > 0 {
		c := parts[0]
		parts = parts[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		cur := stack[len(stack)-1]
		if !cur.IsFolder {
			return nil, pathError(op, name, ErrNotDir)
		}
		u, err := nameToHFS(c)
		if err != nil {
			return nil, pathError(op, name, fs.ErrNotExist)
		}
		r, err := v.lookup(ctx, cur.CNID, u)
		if err != nil {
			return nil, pathError(op, name, err)
		}
		if r == nil || cur.CNID == CNIDRootFolder && v.hidden(r.CNID) {
			return nil, pathError(op, name, fs.ErrNotExist)
		}
		if r, err = v.resolveHardLink(ctx, r); err != nil {
			return nil, pathError(op, name, err)
		}
		if r.isSymlink() && (len(parts) > 0 || follow) {
			if hops++; hops > MaxSymlinkHops {
				return nil, pathError(op, name, ErrLinkLoop)
			}
			target, err := v.readLink(ctx, r)
			if err != nil {
				return nil, pathError(op, name, err)
			}
			if target == "" {
				return nil, pathError(op, name, fs.ErrNotExist)
			}
			if strings.HasPrefix(target, "/") {
				stack = stack[:1]
			}
			parts = append(strings.Split(target, "/"), parts...)
			continue
		}
		stack = append(stack, r)
	}
	return stack[len(stack)-1], nil
}

// hidden reports whether a root folder entry is metadata macOS does not list.
func (v *volume) hidden(cnid uint32) bool {
	if v.opts.ShowPrivate {
		return false
	}
	return cnid == v.privateDir && cnid != 0 || cnid == v.privateDirDir && cnid != 0 ||
		cnid == v.journalFile && cnid != 0 || cnid == v.journalInfo && cnid != 0
}

// fileInfo is the fs.FileInfo of a record.
type fileInfo struct {
	name string
	rec  *Record
	size int64
}

func (fi *fileInfo) Name() string       { return fi.name }
func (fi *fileInfo) Size() int64        { return fi.size }
func (fi *fileInfo) Mode() fs.FileMode  { return fi.rec.Mode() }
func (fi *fileInfo) ModTime() time.Time { return fi.rec.Modified() }
func (fi *fileInfo) IsDir() bool        { return fi.rec.IsFolder }
func (fi *fileInfo) Sys() any           { return fi.rec }
func (fi *fileInfo) String() string     { return fs.FormatFileInfo(fi) }

// info builds the FileInfo of r: the size is the data fork's, or the uncompressed size of a
// compressed file.
func (v *volume) info(ctx context.Context, name string, r *Record) (*fileInfo, error) {
	fi := &fileInfo{name: name, rec: r}
	if r.IsFolder {
		return fi, nil
	}
	fi.size = int64(r.DataFork.LogicalSize)
	if r.Compressed() {
		a, err := v.attr(ctx, r.CNID, XattrDecmpfs)
		if err != nil {
			return nil, err
		}
		if a != nil {
			raw, err := v.readAttr(ctx, r.CNID, a, maxDecmpfsAttr)
			if err != nil {
				return nil, err
			}
			h, err := decodeDecmpfs(raw)
			if err != nil {
				return nil, fmt.Errorf("CNID %d: %w", r.CNID, err)
			}
			fi.size = int64(min(h.UncompressedSize, 1<<62))
		}
	}
	return fi, nil
}

// openData returns a reader of a file's contents: the data fork, or the decompressed data of a
// compressed file.
func (v *volume) openData(ctx context.Context, r *Record) (io.ReaderAt, error) {
	if r.Compressed() {
		a, err := v.attr(ctx, r.CNID, XattrDecmpfs)
		if err != nil {
			return nil, err
		}
		if a != nil {
			return v.openDecmpfs(ctx, r, a)
		}
	}
	f, err := v.newFork(fmt.Sprintf("data fork of CNID %d", r.CNID), r.DataFork, v.overflow(r.CNID, forkData))
	if err != nil {
		return nil, err
	}
	return forkReader{ctx: ctx, f: f}, nil
}

func baseName(name string) string {
	if name == "." {
		return "."
	}
	return path.Base(name)
}

// Open opens the named file or directory, following symbolic links. A file implements
// io.ReaderAt and io.Seeker; a directory implements fs.ReadDirFile.
func (v *Volume) Open(name string) (fs.File, error) {
	r, err := v.resolve("open", name, true)
	if err != nil {
		return nil, err
	}
	return v.open(name, r)
}

func (v *Volume) open(name string, r *Record) (fs.File, error) {
	fi, err := v.info(v.ctx, baseName(name), r)
	if err != nil {
		return nil, pathError("open", name, err)
	}
	if r.IsFolder {
		return &dir{v: v, path: name, info: fi}, nil
	}
	rd, err := v.openData(v.ctx, r)
	if err != nil {
		return nil, pathError("open", name, err)
	}
	return &File{path: name, info: fi, r: rd}, nil
}

// Stat returns the FileInfo of the named file, following symbolic links. Its Sys method returns
// the *Record.
func (v *Volume) Stat(name string) (fs.FileInfo, error) {
	r, err := v.resolve("stat", name, true)
	if err != nil {
		return nil, err
	}
	fi, err := v.info(v.ctx, baseName(name), r)
	if err != nil {
		return nil, pathError("stat", name, err)
	}
	return fi, nil
}

// Lstat returns the FileInfo of the named file without following a final symbolic link.
func (v *Volume) Lstat(name string) (fs.FileInfo, error) {
	r, err := v.resolve("lstat", name, false)
	if err != nil {
		return nil, err
	}
	fi, err := v.info(v.ctx, baseName(name), r)
	if err != nil {
		return nil, pathError("lstat", name, err)
	}
	return fi, nil
}

// ReadLink returns the target of the named symbolic link.
func (v *Volume) ReadLink(name string) (string, error) {
	r, err := v.resolve("readlink", name, false)
	if err != nil {
		return "", err
	}
	if !r.isSymlink() {
		return "", pathError("readlink", name, fs.ErrInvalid)
	}
	t, err := v.readLink(v.ctx, r)
	if err != nil {
		return "", pathError("readlink", name, err)
	}
	return t, nil
}

// ReadFile reads the named file, following symbolic links.
func (v *Volume) ReadFile(name string) ([]byte, error) {
	r, err := v.resolve("open", name, true)
	if err != nil {
		return nil, err
	}
	if r.IsFolder {
		return nil, pathError("read", name, errIsDir)
	}
	fi, err := v.info(v.ctx, baseName(name), r)
	if err != nil {
		return nil, pathError("read", name, err)
	}
	rd, err := v.openData(v.ctx, r)
	if err != nil {
		return nil, pathError("read", name, err)
	}
	b := make([]byte, fi.size)
	n, err := rd.ReadAt(b, 0)
	if err != nil && !(err == io.EOF && int64(n) == fi.size) {
		return nil, pathError("read", name, err)
	}
	return b, nil
}

// ReadDir reads the named directory and returns its entries sorted by name.
func (v *Volume) ReadDir(name string) ([]fs.DirEntry, error) {
	r, err := v.resolve("readdir", name, true)
	if err != nil {
		return nil, err
	}
	if !r.IsFolder {
		return nil, pathError("readdir", name, ErrNotDir)
	}
	d := &dir{v: v, path: name, info: &fileInfo{name: baseName(name), rec: r}}
	es, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(es, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return es, nil
}

var errIsDir = errors.New("is a directory")

// File is an open file. It implements io.Reader, io.ReaderAt and io.Seeker.
type File struct {
	path   string
	info   *fileInfo
	r      io.ReaderAt
	off    int64
	closed bool
}

func (f *File) Stat() (fs.FileInfo, error) {
	if f.closed {
		return nil, pathError("stat", f.path, fs.ErrClosed)
	}
	return f.info, nil
}

func (f *File) Read(p []byte) (int, error) {
	if f.closed {
		return 0, pathError("read", f.path, fs.ErrClosed)
	}
	if f.off >= f.info.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if rem := f.info.size - f.off; int64(len(p)) > rem {
		p = p[:rem]
	}
	n, err := f.r.ReadAt(p, f.off)
	f.off += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

func (f *File) ReadAt(p []byte, off int64) (int, error) {
	if f.closed {
		return 0, pathError("read", f.path, fs.ErrClosed)
	}
	if off < 0 {
		return 0, pathError("read", f.path, errInvalidOffset)
	}
	if off >= f.info.size {
		return 0, io.EOF
	}
	var eof error
	if rem := f.info.size - off; int64(len(p)) > rem {
		p, eof = p[:rem], io.EOF
	}
	n, err := f.r.ReadAt(p, off)
	if err == nil {
		err = eof
	}
	return n, err
}

func (f *File) Seek(offset int64, whence int) (int64, error) {
	if f.closed {
		return 0, pathError("seek", f.path, fs.ErrClosed)
	}
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += f.off
	case io.SeekEnd:
		offset += f.info.size
	default:
		return 0, pathError("seek", f.path, fs.ErrInvalid)
	}
	if offset < 0 {
		return 0, pathError("seek", f.path, errInvalidOffset)
	}
	f.off = offset
	return offset, nil
}

func (f *File) Close() error {
	if f.closed {
		return pathError("close", f.path, fs.ErrClosed)
	}
	f.closed = true
	return nil
}

// dir is an open directory.
type dir struct {
	v      *Volume
	path   string
	info   *fileInfo
	it     *dirIter
	eof    bool
	closed bool
}

func (d *dir) Stat() (fs.FileInfo, error) {
	if d.closed {
		return nil, pathError("stat", d.path, fs.ErrClosed)
	}
	return d.info, nil
}

func (d *dir) Read([]byte) (int, error) { return 0, pathError("read", d.path, errIsDir) }

func (d *dir) Close() error {
	if d.closed {
		return pathError("close", d.path, fs.ErrClosed)
	}
	d.closed = true
	return nil
}

// ReadDir returns the directory's entries in catalog order, n at a time as fs.ReadDirFile
// specifies.
func (d *dir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.closed {
		return nil, pathError("readdir", d.path, fs.ErrClosed)
	}
	ctx := d.v.ctx
	if d.it == nil && !d.eof {
		it, err := d.v.list(ctx, d.info.rec.CNID)
		if err != nil {
			return nil, pathError("readdir", d.path, err)
		}
		d.it = it
	}
	var out []fs.DirEntry
	for !d.eof && (n <= 0 || len(out) < n) {
		r, err := d.it.next(ctx)
		if err != nil {
			return out, pathError("readdir", d.path, err)
		}
		if r == nil {
			d.eof = true
			break
		}
		if d.info.rec.CNID == CNIDRootFolder && d.v.hidden(r.CNID) {
			continue
		}
		if r, err = d.v.resolveHardLink(ctx, r); err != nil {
			return out, pathError("readdir", d.path, err)
		}
		out = append(out, &dirEntry{v: d.v, rec: r})
	}
	if n > 0 && len(out) == 0 {
		return nil, io.EOF
	}
	return out, nil
}

type dirEntry struct {
	v   *Volume
	rec *Record
}

func (e *dirEntry) Name() string               { return e.rec.Name }
func (e *dirEntry) IsDir() bool                { return e.rec.IsFolder }
func (e *dirEntry) Type() fs.FileMode          { return e.rec.Mode().Type() }
func (e *dirEntry) Info() (fs.FileInfo, error) { return e.v.info(e.v.ctx, e.rec.Name, e.rec) }
func (e *dirEntry) String() string             { return fs.FormatDirEntry(e) }

// OpenResourceFork opens the resource fork of the named file, following symbolic links.
func (v *Volume) OpenResourceFork(name string) (*File, error) {
	r, err := v.resolve("open", name, true)
	if err != nil {
		return nil, err
	}
	if r.IsFolder {
		return nil, pathError("open", name, errIsDir)
	}
	f, err := v.newFork(fmt.Sprintf("resource fork of CNID %d", r.CNID), r.ResourceFork, v.overflow(r.CNID, forkResource))
	if err != nil {
		return nil, pathError("open", name, err)
	}
	return &File{path: name, info: &fileInfo{name: baseName(name), rec: r, size: f.size}, r: forkReader{ctx: v.ctx, f: f}}, nil
}
