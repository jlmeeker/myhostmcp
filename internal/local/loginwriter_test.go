package local

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// TestTshLoginWriterNoCarriageReturn verifies the writer never emits a bare
// carriage return (or any other C0 control char) to the logger, regardless of
// how tsh chunks its output. A stray \r reaching a live TUI (e.g. pi)
// repositions the cursor and corrupts rendering.
func TestTshLoginWriterNoCarriageReturn(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   []string // substrings expected to appear, in order
	}{
		{
			name:   "crlf lines",
			chunks: []string{"If browser window does not open automatically:\r\n http://127.0.0.1:1234/uuid\r\n"},
			want:   []string{"If browser window does not open automatically:", "http://127.0.0.1:1234/uuid"},
		},
		{
			name:   "bare cr progress then newline",
			chunks: []string{"Waiting...\rWaiting..\rWaiting.\rDone\n"},
			want:   []string{"Waiting", "Done"},
		},
		{
			name:   "split mid-line across writes",
			chunks: []string{"open it by clicking on the link:", "\r\n http://x/y\r\n"},
			want:   []string{"open it by clicking on the link:", "http://x/y"},
		},
		{
			name:   "url arrives on its own without trailing newline then flushed",
			chunks: []string{" http://127.0.0.1:39869/f5c31281\n"},
			want:   []string{"http://127.0.0.1:39869/f5c31281"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := &tshLoginWriter{log: log.New(&buf, "", 0)}
			for _, c := range tc.chunks {
				if _, err := w.Write([]byte(c)); err != nil {
					t.Fatalf("Write: %v", err)
				}
			}
			out := buf.String()
			if strings.ContainsRune(out, '\r') {
				t.Errorf("output contains a carriage return:\n%q", out)
			}
			for _, r := range out {
				if r == '\n' || r == '\t' {
					continue
				}
				if r < 0x20 || r == 0x7f {
					t.Errorf("output contains control char %#x:\n%q", r, out)
				}
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("expected output to contain %q; got:\n%s", want, out)
				}
			}
		})
	}
}
