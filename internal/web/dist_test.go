package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// The dashboard served by the Go binary is whatever is embedded here, so the
// committed dist must be a complete build of web/: every asset referenced by
// index.html exists, no stale bundle is left behind, and the native-allowance
// UI is present.
func TestEmbeddedDistIsCompleteAndCurrent(t *testing.T) {
	index, err := fs.ReadFile(DistFS, "dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`/dashboard/(assets/[A-Za-z0-9._-]+)`).FindAllStringSubmatch(string(index), -1)
	if len(refs) < 2 {
		t.Fatalf("index.html should reference a js and a css bundle, got %v", refs)
	}
	referenced := map[string]bool{}
	for _, m := range refs {
		referenced["dist/"+m[1]] = true
		if _, err := fs.Stat(DistFS, "dist/"+m[1]); err != nil {
			t.Errorf("index.html references missing asset %s", m[1])
		}
	}
	entries, err := fs.ReadDir(DistFS, "dist/assets")
	if err != nil {
		t.Fatal(err)
	}
	var js string
	for _, e := range entries {
		p := "dist/assets/" + e.Name()
		if !referenced[p] {
			t.Errorf("stale unreferenced asset %s (old hash must be removed)", p)
		}
		if strings.HasSuffix(e.Name(), ".js") {
			b, _ := fs.ReadFile(DistFS, p)
			js = string(b)
		}
	}
	for _, marker := range []string{"credit-overview", "credit-native", "credit-legacy"} {
		if !strings.Contains(js, marker) {
			t.Errorf("embedded bundle lacks %q: dist was not rebuilt from web/src", marker)
		}
	}
}
