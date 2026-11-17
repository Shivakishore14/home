package transcoder

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"taskengine/pkg/plugin"
)

func TestCheckInputFile(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "movie.avi")
	out := filepath.Join(dir, "movie.mp4")

	// Source + output both missing, dir exists -> permanent.
	err := checkInputFile(in, out)
	var perm *plugin.PermanentError
	if !errors.As(err, &perm) {
		t.Fatalf("expected PermanentError, got %v", err)
	}

	// Output exists -> skip.
	if err := os.WriteFile(out, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	var skip *plugin.SkipError
	if err := checkInputFile(in, out); !errors.As(err, &skip) {
		t.Fatalf("expected SkipError, got %v", err)
	}

	// Source exists -> proceed.
	if err := os.WriteFile(in, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := checkInputFile(in, out); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	// Source is already .mp4 and missing -> not treated as "already transcoded".
	mp4 := filepath.Join(dir, "other.mp4")
	if err := checkInputFile(mp4, mp4); !errors.As(err, &perm) {
		t.Fatalf("expected PermanentError for missing mp4 source, got %v", err)
	}

	// Parent dir missing (e.g. share unmounted) -> retryable, not skip/permanent.
	gone := filepath.Join(dir, "nope", "movie.avi")
	err = checkInputFile(gone, filepath.Join(dir, "nope", "movie.mp4"))
	if err == nil || errors.As(err, &perm) || errors.As(err, &skip) {
		t.Fatalf("expected plain retryable error, got %v", err)
	}
}
