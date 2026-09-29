package lzvn

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestCompressionTool decodes LZVN streams produced by macOS's
// compression_tool, which has no raw LZVN mode but wraps inputs of 8 to 4095
// bytes as a single "bvxn" block of an LZFSE stream. It is skipped where the
// tool is absent.
func TestCompressionTool(t *testing.T) {
	tool, err := exec.LookPath("compression_tool")
	if err != nil {
		t.Skip("compression_tool not available")
	}
	dir := t.TempDir()
	p := prng(42)
	for i := range 40 {
		size := 8 + p.intn(4088)
		var data []byte
		switch i % 4 {
		case 0:
			data = genText(uint64(i), size)
		case 1:
			data = genMixed(uint64(i), size)
		case 2:
			data = genRepeat(1+p.intn(300), size)
		default:
			data = genRandom(uint64(i), size)
		}
		src := filepath.Join(dir, "in")
		out := src + ".lzfse"
		if err := os.WriteFile(src, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if b, err := exec.Command(tool, "-encode", "-a", "lzfse", "-i", src, "-o", out).CombinedOutput(); err != nil {
			t.Fatalf("compression_tool: %v: %s", err, b)
		}
		comp, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if len(comp) < 12 || string(comp[:4]) != "bvxn" {
			t.Logf("input %d (%d bytes): stored rather than LZVN-compressed, skipped", i, size)
			continue
		}
		nRaw := binary.LittleEndian.Uint32(comp[4:])
		nPayload := binary.LittleEndian.Uint32(comp[8:])
		if int(nRaw) != size || 12+int(nPayload) > len(comp) {
			t.Fatalf("input %d: unexpected bvxn header raw=%d payload=%d", i, nRaw, nPayload)
		}
		dst := make([]byte, size)
		if n, err := Decode(dst, comp[12:12+nPayload]); err != nil || n != size || !bytes.Equal(dst, data) {
			t.Fatalf("input %d (%d bytes): Decode = %d, %v", i, size, n, err)
		}
	}
}
