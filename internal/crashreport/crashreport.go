package crashreport

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

func Dir() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(".selfmind", "crashes")
	}
	return filepath.Join(home, ".selfmind", "crashes")
}

func Write(recovered any) (string, error) {
	dir := Dir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, time.Now().UTC().Format("20060102T150405.000000000Z")+".log")
	body := fmt.Sprintf("SelfMind panic at %s\n\n%v\n\n%s", time.Now().UTC().Format(time.RFC3339Nano), recovered, debug.Stack())
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		return "", err
	}
	return path, nil
}

func Latest() (string, bool) {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		return "", false
	}
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		paths = append(paths, filepath.Join(Dir(), entry.Name()))
	}
	if len(paths) == 0 {
		return "", false
	}
	sort.Strings(paths)
	return paths[len(paths)-1], true
}

// ConsumeNotice returns the newest crash report once. It records only the
// report filename, so the next CLI start does not repeatedly show the same
// notice. The crash itself remains on disk until the user removes it.
func ConsumeNotice() (string, bool) {
	path, ok := Latest()
	if !ok {
		return "", false
	}
	marker := filepath.Join(Dir(), ".last-noticed")
	if data, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(data)) == filepath.Base(path) {
		return "", false
	}
	tmp := marker + ".tmp"
	if err := os.WriteFile(tmp, []byte(filepath.Base(path)+"\n"), 0600); err != nil {
		return path, true
	}
	if err := os.Rename(tmp, marker); err != nil {
		_ = os.Remove(tmp)
		return path, true
	}
	return path, true
}
