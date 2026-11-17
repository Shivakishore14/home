package transcoder

import (
	"strings"
	"testing"
)

func TestLineScannerHandlesCRAndLongRuns(t *testing.T) {
	// ffmpeg-style stderr: many \r-redrawn status lines with no \n, well past 64KB.
	var b strings.Builder
	b.WriteString("header\n")
	for i := 0; i < 5000; i++ {
		b.WriteString("frame= 1234 fps=200 q=21.0 size=  10240kB time=00:01:00.00 bitrate=1000kbits/s speed=8x\r")
	}
	b.WriteString("done\r\nlast")
	sc := newLineScanner(strings.NewReader(b.String()))
	n := 0
	var last string
	for sc.Scan() {
		n++
		last = sc.Text()
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	if n != 5003 || last != "last" {
		t.Fatalf("got %d lines, last=%q", n, last)
	}
}
