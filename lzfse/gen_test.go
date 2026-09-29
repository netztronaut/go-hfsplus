package lzfse

// Deterministic generators for the inputs of the checked-in test vectors.
// The vectors store only compressed data; the tests regenerate the original
// bytes with these functions, so they must never change (a change shows up
// as a mismatch in TestVectors). The same generators exist in the lzvn
// package's tests.

// prng is SplitMix64: tiny, and fully specified here so that its output
// cannot change with the Go release.
type prng uint64

func (p *prng) next() uint64 {
	*p += 0x9e3779b97f4a7c15
	z := uint64(*p)
	z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
	z = (z ^ z>>27) * 0x94d049bb133111eb
	return z ^ z>>31
}

func (p *prng) intn(n int) int { return int(p.next() % uint64(n)) }

var genWords = []string{
	"the", "of", "and", "a", "to", "in", "is", "you", "that", "it", "he",
	"was", "for", "on", "are", "as", "with", "his", "they", "at", "be",
	"this", "have", "from", "or", "one", "had", "by", "word", "but", "not",
	"what", "all", "were", "we", "when", "your", "can", "said", "there",
	"use", "an", "each", "which", "she", "do", "how", "their", "if", "will",
	"up", "other", "about", "out", "many", "then", "them", "these", "so",
	"some", "her", "would", "make", "like", "him", "into", "time", "has",
	"look", "two", "more", "write", "go", "see", "number", "no", "way",
	"could", "people", "my", "than", "first", "water", "been", "call",
	"who", "oil", "its", "now", "find", "long", "down", "day", "did", "get",
	"come", "made", "may", "part", "volume", "catalog", "extent", "fork",
	"compressed", "resource", "partition", "block", "header", "node",
}

// genText returns n bytes of word salad with punctuation and line breaks.
func genText(seed uint64, n int) []byte {
	p := prng(seed)
	out := make([]byte, 0, n+16)
	col := 0
	for len(out) < n {
		w := genWords[p.intn(len(genWords))]
		if p.intn(9) == 0 {
			w = string(rune('A'+p.intn(26))) + w
		}
		out = append(out, w...)
		col += len(w)
		switch r := p.intn(20); {
		case r == 0:
			out = append(out, '.', ' ')
		case r == 1:
			out = append(out, ',', ' ')
		case col > 70:
			out = append(out, '\n')
			col = 0
		default:
			out = append(out, ' ')
		}
	}
	return out[:n]
}

func genRandom(seed uint64, n int) []byte {
	p := prng(seed)
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(p.next() >> 56)
	}
	return out
}

func genRepeat(period, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte('!' + (i%period)*7%90)
	}
	return out
}

// genMixed interleaves text, noise, byte runs and near and far copies of
// earlier output (some with scattered substitutions, so that encoders use
// repeated distances), to exercise many literals and matches.
func genMixed(seed uint64, n int) []byte {
	p := prng(seed)
	base := genText(seed^0x5a5a, 40000)
	out := make([]byte, 0, n+8192)
	for len(out) < n {
		switch p.intn(16) {
		case 0: // noise
			for k := 1 + p.intn(48); k > 0; k-- {
				out = append(out, byte(p.next()>>56))
			}
		case 1: // run of one byte
			b := byte(p.next() >> 56)
			for k := 3 + p.intn(2000); k > 0; k-- {
				out = append(out, b)
			}
		case 2, 3, 4, 5, 6, 7, 8, 9, 10, 11: // copy from far back, lightly mutated
			if len(out) < 1024 {
				continue
			}
			from := p.intn(len(out) - 512)
			k := 64 + p.intn(4000)
			for i := 0; i < k && from+i < len(out); i++ {
				b := out[from+i]
				if p.intn(97) == 0 {
					b ^= 0x20
				}
				out = append(out, b)
			}
		default: // text
			from := p.intn(len(base) - 2048)
			out = append(out, base[from:from+16+p.intn(2000)]...)
		}
	}
	return out[:n]
}
