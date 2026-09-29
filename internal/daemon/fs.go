package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/kube"
)

const fsListTimeout = 15 * time.Second

// listDirsScript prints the absolute path of the directory in $1 (~ and
// relative paths from the home), then the names of the directories in it.
const listDirsScript = `p=$1
case $p in "~") p=$HOME ;; "~/"*) p=$HOME/${p#"~/"} ;; /*) ;; *) p=$HOME/$p ;; esac
cd -- "$p" 2>/dev/null || { echo "no such directory: $1" >&2; exit 1; }
pwd
for d in .[!.]* ..?* *; do [ -d "$d" ] && printf '%s\n' "$d"; done
exit 0`

func (d *Daemon) handleFSList(ctx context.Context, params json.RawMessage) (any, error) {
	p, err := decode[api.FSListParams](params)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, fsListTimeout)
	defer cancel()
	m, _, release, err := d.hostMaster(ctx, p.Host)
	if err != nil {
		return nil, err
	}
	defer release()
	path := p.Path
	if path == "" {
		path = "~"
	}
	out, err := m.Run(ctx, "", "sh -c "+kube.Quote(listDirsScript)+" tether-ls "+kube.Quote(path))
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "/") {
		return nil, errors.New("unexpected output listing " + path)
	}
	res := api.FSListResult{Path: lines[0], Dirs: []string{}}
	for _, l := range lines[1:] {
		if l != "" {
			res.Dirs = append(res.Dirs, l)
		}
	}
	slices.Sort(res.Dirs)
	return res, nil
}
