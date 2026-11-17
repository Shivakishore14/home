package transcoder

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"taskengine/pkg/plugin"
)

func TestLibraryDir(t *testing.T) {
	cases := []struct {
		in, incoming, want string
		staged             bool
	}{
		{"/m/_incoming/Movies/X", "_incoming", "/m/Movies/X", true},
		{"/m/Movies/X", "_incoming", "/m/Movies/X", false},
		{"/m/_incoming/Movies/X", "", "/m/_incoming/Movies/X", false},
		{"/m/_incoming", "_incoming", "/m", true},
	}
	for _, c := range cases {
		got, staged := libraryDir(c.in, c.incoming)
		if got != c.want || staged != c.staged {
			t.Errorf("libraryDir(%q,%q) = %q,%v; want %q,%v", c.in, c.incoming, got, staged, c.want, c.staged)
		}
	}
}

func TestMoveSidecarsAndPrune(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "_incoming", "Movies", "Foo (2020)")
	dst := filepath.Join(root, "Movies", "Foo (2020)")
	for _, d := range []string{src, dst} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"foo.en.srt", "foo.nfo"} {
		if err := os.WriteFile(filepath.Join(src, f), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	moveSidecars(src, dst, "foo")
	for _, f := range []string{"foo.en.srt", "foo.nfo"} {
		if _, err := os.Stat(filepath.Join(dst, f)); err != nil {
			t.Errorf("%s not moved: %v", f, err)
		}
	}

	pruneEmptyDirs(src, "_incoming")
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("empty title dir not pruned")
	}
	if _, err := os.Stat(filepath.Join(root, "_incoming", "Movies")); err != nil {
		t.Errorf("library drop folder _incoming/Movies should be kept: %v", err)
	}
}

func TestExecuteIncomingToLibrary(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed; skipping test")
	}
	for _, scratch := range []bool{false, true} {
		root := t.TempDir()
		src := filepath.Join(root, "_incoming", "Movies", "Foo (2020)")
		if err := os.MkdirAll(src, 0755); err != nil {
			t.Fatal(err)
		}
		input := filepath.Join(src, "foo.mkv")
		gen := exec.Command(ffmpegPath, "-y", "-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=10", "-c:v", "libx264", input)
		if out, err := gen.CombinedOutput(); err != nil {
			t.Fatalf("generate: %v (%s)", err, out)
		}
		_ = os.WriteFile(filepath.Join(src, "foo.en.srt"), []byte("1\n"), 0644)

		params := map[string]interface{}{
			"target_codec": "libx264", "preset": "ultrafast", "crf": 28,
			"incoming_dir": "_incoming",
		}
		if scratch {
			params["scratch_dir"] = t.TempDir()
		}
		pj, _ := json.Marshal(params)
		w := &VideoTranscoderWorkerPlugin{}
		_ = w.Init(context.Background(), nil)
		err = w.Execute(context.Background(), plugin.TaskPayload{ID: "t1", TargetFile: input, Params: pj}, &mockReporter{})
		if err != nil {
			t.Fatalf("scratch=%v: Execute: %v", scratch, err)
		}

		lib := filepath.Join(root, "Movies", "Foo (2020)")
		for _, f := range []string{"foo.mp4", "foo.en.srt"} {
			if _, err := os.Stat(filepath.Join(lib, f)); err != nil {
				t.Errorf("scratch=%v: %s missing from library: %v", scratch, f, err)
			}
		}
		if !readTranscodeCache(lib)["foo.mp4"] {
			t.Errorf("scratch=%v: foo.mp4 not in library .transcode_cache", scratch)
		}
		if _, err := os.Stat(src); !os.IsNotExist(err) {
			t.Errorf("scratch=%v: incoming title dir not cleaned up", scratch)
		}
		if _, err := os.Stat(filepath.Join(root, "_incoming", "Movies")); err != nil {
			t.Errorf("scratch=%v: drop folder removed: %v", scratch, err)
		}
	}
}
