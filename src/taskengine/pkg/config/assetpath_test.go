package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetAssetFilePathConfinedToTasksDir(t *testing.T) {
	root := t.TempDir()
	tasks := filepath.Join(root, "tasks")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(tasks, "assets", "x"), 0755))
	must(os.MkdirAll(filepath.Join(root, "data"), 0755))
	must(os.WriteFile(filepath.Join(tasks, "assets", "x", "a.py"), []byte("a"), 0644))
	must(os.WriteFile(filepath.Join(root, "data", "taskengine.db"), []byte("secret"), 0644))
	must(os.WriteFile(filepath.Join(root, "taskengine"), []byte("bin"), 0755))

	m, err := NewManager(tasks)
	must(err)

	for _, ok := range []string{"tasks/assets/x/a.py", "assets/x/a.py"} {
		if _, err := m.GetAssetFilePath(ok); err != nil {
			t.Errorf("%s should resolve: %v", ok, err)
		}
	}
	for _, bad := range []string{"data/taskengine.db", "taskengine", "../data/taskengine.db", "tasks/../data/taskengine.db", "/etc/passwd", "tasks/assets"} {
		if p, err := m.GetAssetFilePath(bad); err == nil {
			t.Errorf("%s must not resolve (got %s)", bad, p)
		}
	}
}
