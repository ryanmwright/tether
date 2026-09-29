package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ryanmwright/tether/internal/api"
)

func TestFSList(t *testing.T) {
	h := startWithSSH(t, "")
	dir := t.TempDir()
	for _, d := range []string{"b", "a", ".hidden", "with space"} {
		os.Mkdir(filepath.Join(dir, d), 0o755)
	}
	os.WriteFile(filepath.Join(dir, "file"), nil, 0o644)

	var res api.FSListResult
	h.call(t, api.MethodFSList, api.FSListParams{Host: "dev", Path: dir}, &res)
	if want := []string{".hidden", "a", "b", "with space"}; res.Path != dir || !reflect.DeepEqual(res.Dirs, want) {
		t.Errorf("fs.list = %+v", res)
	}
	if host(h.status(t), "dev").State != api.StateDown {
		t.Error("listing left the host connected")
	}

	home, _ := os.UserHomeDir()
	h.call(t, api.MethodFSList, api.FSListParams{Host: "dev", Path: "~"}, &res)
	if res.Path != home {
		t.Errorf("~ = %q, want %q", res.Path, home)
	}

	err := h.c.Call(context.Background(), api.MethodFSList, api.FSListParams{Host: "dev", Path: filepath.Join(dir, "nope")}, &res)
	if err == nil || !strings.Contains(err.Error(), "no such directory") {
		t.Errorf("missing directory: %v", err)
	}
}
