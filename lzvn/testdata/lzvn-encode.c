// lzvn-encode compresses a file to a raw LZVN stream with macOS's
// libcompression. It was used to produce the *.lzvn test vectors; it is not
// part of the Go build. Algorithm 0x900 is libcompression's undocumented
// identifier for raw LZVN.
//
//	clang -O2 -o lzvn-encode lzvn-encode.c -lcompression
//	./lzvn-encode input output.lzvn
#include <compression.h>
#include <stdio.h>
#include <stdlib.h>

int main(int argc, char **argv) {
  if (argc != 3) {
    fprintf(stderr, "usage: %s input output\n", argv[0]);
    return 2;
  }
  FILE *f = fopen(argv[1], "rb");
  if (!f) { perror(argv[1]); return 1; }
  fseek(f, 0, SEEK_END);
  long n = ftell(f);
  fseek(f, 0, SEEK_SET);
  unsigned char *in = malloc(n + 1);
  if (fread(in, 1, n, f) != (size_t)n) { perror("read"); return 1; }
  fclose(f);
  size_t cap = 2 * (size_t)n + 4096;
  unsigned char *out = malloc(cap);
  size_t m = compression_encode_buffer(out, cap, in, n, NULL, (compression_algorithm)0x900);
  if (m == 0) { fprintf(stderr, "encode failed\n"); return 1; }
  FILE *o = fopen(argv[2], "wb");
  if (!o || fwrite(out, 1, m, o) != m || fclose(o) != 0) { perror(argv[2]); return 1; }
  return 0;
}
