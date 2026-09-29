package lzfse

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestCompressionTool round-trips larger inputs than the checked-in vectors
// through macOS's compression_tool. It is skipped where the tool is absent.
func TestCompressionTool(t *testing.T) {
	tool, err := exec.LookPath("compression_tool")
	if err != nil {
		t.Skip("compression_tool not available")
	}
	if testing.Short() {
		t.Skip("skipped in short mode")
	}
	inputs := []struct {
		name string
		data []byte
	}{
		{"mixed-4m", genMixed(21, 4<<20)},
		{"mixed-2m", genMixed(24, 2<<20+17)},
		{"text-3m", genText(22, 3<<20)},
		{"random-1m", genRandom(23, 1<<20)},
		{"zeros-9m", make([]byte, 9<<20)},
		{"text-4096", genText(25, 4096)},
		{"text-4097", genText(26, 4097)},
		{"random-8", genRandom(27, 8)},
	}
	dir := t.TempDir()
	for _, in := range inputs {
		t.Run(in.name, func(t *testing.T) {
			src := filepath.Join(dir, in.name)
			out := src + ".lzfse"
			if err := os.WriteFile(src, in.data, 0o644); err != nil {
				t.Fatal(err)
			}
			if b, err := exec.Command(tool, "-encode", "-a", "lzfse", "-i", src, "-o", out).CombinedOutput(); err != nil {
				t.Fatalf("compression_tool: %v: %s", err, b)
			}
			comp, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%d -> %d bytes, blocks %s", len(in.data), len(comp), summarize(blockKinds(comp)))
			dst := make([]byte, len(in.data))
			if n, err := Decode(dst, comp); err != nil || n != len(dst) || !bytes.Equal(dst, in.data) {
				t.Fatalf("Decode = %d, %v", n, err)
			}
		})
	}
}

// summarize shortens a block-kind string such as "2222$" to "2x4 $".
func summarize(kinds string) string {
	var out []byte
	for i := 0; i < len(kinds); {
		j := i
		for j < len(kinds) && kinds[j] == kinds[i] {
			j++
		}
		if len(out) > 0 {
			out = append(out, ' ')
		}
		out = append(out, kinds[i])
		if j-i > 1 {
			out = append(out, []byte("x"+itoa(j-i))...)
		}
		i = j
	}
	return string(out)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
