package hfsplus

import (
	"errors"
	"fmt"
	"io/fs"
)

// Errors. Every error this package returns wraps one of these, or fs.ErrNotExist, fs.ErrInvalid,
// or the caller's context error, so errors.Is works on it.
var (
	// ErrNotHFSPlus is returned by Open when the reader holds neither an HFS+ or HFSX volume
	// header nor an HFS wrapper around one.
	ErrNotHFSPlus = errors.New("hfsplus: not an HFS+ or HFSX volume")
	// ErrCorrupt is returned, inside a *CorruptError, for a structure that is inconsistent. A
	// volume read while a guest has it mounted read-write can produce it too.
	ErrCorrupt = errors.New("hfsplus: corrupt volume")
	// ErrUnsupported is returned, inside an *UnsupportedError, for a feature the reader does not
	// implement, such as a decmpfs compression type it does not know.
	ErrUnsupported = errors.New("hfsplus: unsupported feature")
	// ErrJournalNotReplayed is returned by Open with Options.Journal set to Refuse when the
	// journal holds transactions that were never written to their home locations.
	ErrJournalNotReplayed = errors.New("hfsplus: journal holds unreplayed transactions")
	// ErrLinkLoop is returned when resolving a path follows more than MaxSymlinkHops symbolic
	// links, which is what a loop does.
	ErrLinkLoop = errors.New("hfsplus: too many levels of symbolic links")
)

// CorruptError describes a structure that failed a consistency check.
type CorruptError struct {
	Structure string // "volume header", "catalog B-tree", "journal", "decmpfs", ...
	Node      uint32 // B-tree node number, when the structure is a B-tree node
	Offset    int64  // byte offset on the volume (or in the fork, as Detail says), -1 if none
	Detail    string
}

func (e *CorruptError) Error() string {
	s := "hfsplus: corrupt " + e.Structure
	if e.Structure != "" && isBTree(e.Structure) {
		s += fmt.Sprintf(" node %d", e.Node)
	}
	if e.Offset >= 0 {
		s += fmt.Sprintf(" at offset %d", e.Offset)
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

func (e *CorruptError) Unwrap() error { return ErrCorrupt }

func isBTree(s string) bool {
	return len(s) > 7 && s[len(s)-7:] == "B-tree"
}

func corrupt(structure string, offset int64, format string, args ...any) error {
	return &CorruptError{Structure: structure, Offset: offset, Detail: fmt.Sprintf(format, args...)}
}

func corruptNode(tree string, node uint32, format string, args ...any) error {
	return &CorruptError{Structure: tree, Node: node, Offset: -1, Detail: fmt.Sprintf(format, args...)}
}

// UnsupportedError names a feature the reader does not implement.
type UnsupportedError struct {
	Feature string
}

func (e *UnsupportedError) Error() string { return "hfsplus: unsupported: " + e.Feature }
func (e *UnsupportedError) Unwrap() error { return ErrUnsupported }

func unsupported(format string, args ...any) error {
	return &UnsupportedError{Feature: fmt.Sprintf(format, args...)}
}

func pathError(op, name string, err error) error {
	return &fs.PathError{Op: op, Path: name, Err: err}
}
