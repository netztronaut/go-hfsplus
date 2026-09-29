package lzfse

import (
	"bytes"
	"errors"
	"testing"
)

func FuzzDecode(f *testing.F) {
	for _, v := range vectors {
		f.Add(readVector(f, v.name), uint32(len(v.gen())))
	}
	f.Add([]byte("bvx$"), uint32(0))
	f.Fuzz(func(t *testing.T, src []byte, size uint32) {
		dst := make([]byte, size%(2<<20))
		n, err := Decode(dst, src)
		if n < 0 || n > len(dst) {
			t.Fatalf("n = %d, len(dst) = %d", n, len(dst))
		}
		if err != nil {
			if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrDstTooSmall) {
				t.Fatalf("error %v wraps neither ErrCorrupt nor ErrDstTooSmall", err)
			}
			return
		}
		// A successful decode is reproducible into a destination of
		// exactly the decoded size.
		exact := make([]byte, n)
		if m, err := Decode(exact, src); err != nil || m != n || !bytes.Equal(exact, dst[:n]) {
			t.Fatalf("redecode into %d bytes = %d, %v", n, m, err)
		}
	})
}
