package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/ryanmwright/tether/internal/api"
)

const browserRows = 8

// pathBrowser lists the directories matching what's typed in a path field,
// here or on a host, like a shell completing a path: the directory part is
// listed, filtered by the rest. ↑/↓ pick one, → opens it, ← goes up.
type pathBrowser struct {
	remote bool // on host, through the daemon; else on this machine
	host   string
	dir    string // the directory part listed, as typed ("~/src/", "")
	loaded bool
	abs    string // where dir is
	dirs   []string
	err    string
	cursor int
}

type dirListMsg struct {
	field    int
	dir, abs string
	dirs     []string
	err      error
}

// splitPath cuts a typed path into the directory to list and the prefix to
// filter it by: "~/sr" → "~/", "sr"; "/etc/" → "/etc/", "".
func splitPath(text string) (dir, prefix string) {
	if text == "~" {
		return "~/", ""
	}
	i := strings.LastIndex(text, "/")
	if i < 0 {
		return "", text
	}
	return text[:i+1], text[i+1:]
}

// matches are the listed directories starting with prefix; hidden ones only
// once a "." is typed.
func (b *pathBrowser) matches(prefix string) []string {
	var out []string
	for _, d := range b.dirs {
		if strings.HasPrefix(d, prefix) && (!strings.HasPrefix(d, ".") || strings.HasPrefix(prefix, ".")) {
			out = append(out, d)
		}
	}
	return out
}

// syncBrowser loads the listing for the focused path field if what's typed
// now names another directory.
func (m *Model) syncBrowser() tea.Cmd {
	if m.mode != modeForm {
		return nil
	}
	i := m.form.focus
	b := m.form.fields[i].browse
	if b == nil {
		return nil
	}
	dir, _ := splitPath(m.form.fields[i].input.Value())
	if b.loaded && dir == b.dir {
		return nil
	}
	b.dir, b.loaded, b.err, b.dirs, b.cursor = dir, true, "", nil, 0
	if b.remote {
		c, host := m.client, b.host
		listed := dir
		if listed == "" {
			listed = "~"
		}
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
			defer cancel()
			var res api.FSListResult
			err := c.Call(ctx, api.MethodFSList, api.FSListParams{Host: host, Path: listed}, &res)
			return dirListMsg{field: i, dir: dir, abs: res.Path, dirs: res.Dirs, err: err}
		}
	}
	return func() tea.Msg {
		abs, dirs, err := listLocalDirs(dir)
		return dirListMsg{field: i, dir: dir, abs: abs, dirs: dirs, err: err}
	}
}

// listLocalDirs lists the directories in dir here: ~ from the home,
// relative paths from the working directory.
func listLocalDirs(dir string) (string, []string, error) {
	home, _ := os.UserHomeDir()
	p := dir
	switch {
	case p == "":
		p, _ = os.Getwd()
	case p == "~/" || p == "~":
		p = home
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(home, p[2:])
	case !filepath.IsAbs(p):
		p, _ = filepath.Abs(p)
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return p, nil, err
	}
	var dirs []string
	for _, e := range entries {
		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			fi, err := os.Stat(filepath.Join(p, e.Name()))
			isDir = err == nil && fi.IsDir()
		}
		if isDir {
			dirs = append(dirs, e.Name())
		}
	}
	slices.Sort(dirs)
	return filepath.Clean(p), dirs, nil
}

func (m Model) gotDirs(msg dirListMsg) Model {
	if m.mode != modeForm || msg.field >= len(m.form.fields) {
		return m
	}
	b := m.form.fields[msg.field].browse
	if b == nil || b.dir != msg.dir {
		return m // stale
	}
	b.abs, b.dirs, b.err = msg.abs, msg.dirs, ""
	if msg.err != nil {
		b.err = msg.err.Error()
	}
	return m
}

// browseKey handles the keys a path field's browser takes; it reports
// whether it took the key.
func (fl *formField) browseKey(key string) bool {
	b := fl.browse
	value := fl.input.Value()
	atEnd := fl.input.Position() == len([]rune(value))
	dir, prefix := splitPath(value)
	matches := b.matches(prefix)
	switch key {
	case "up":
		b.cursor = max(b.cursor-1, 0)
		return true
	case "down":
		b.cursor = min(b.cursor+1, max(len(matches)-1, 0))
		return true
	case "right":
		if !atEnd || b.cursor >= len(matches) {
			return false
		}
		fl.input.SetValue(dir + matches[b.cursor] + "/")
		fl.input.CursorEnd()
		b.cursor = 0
		return true
	case "left":
		if !atEnd || !strings.HasSuffix(value, "/") || value == "/" || value == "~/" {
			return false
		}
		up, _ := splitPath(strings.TrimSuffix(value, "/"))
		fl.input.SetValue(up)
		fl.input.CursorEnd()
		b.cursor = 0
		return true
	}
	return false
}

// view lists the matches under the field.
func (b *pathBrowser) view(value string) []string {
	_, prefix := splitPath(value)
	where := b.abs
	if where == "" {
		where = b.dir
	}
	if b.remote {
		where = b.host + ":" + where
	}
	switch {
	case !b.loaded || b.abs == "" && b.err == "":
		return []string{styleFaint.Render("    listing " + where + "…")}
	case b.err != "":
		return []string{styleWarn.Render("    " + b.err)}
	}
	matches := b.matches(prefix)
	lines := []string{styleFaint.Render(fmt.Sprintf("    in %s: %d director%s", where, len(matches), map[bool]string{true: "y", false: "ies"}[len(matches) == 1]))}
	if len(matches) == 0 {
		return lines
	}
	start := 0
	if b.cursor >= browserRows {
		start = b.cursor - browserRows + 1
	}
	for i := start; i < min(start+browserRows, len(matches)); i++ {
		if i == b.cursor {
			lines = append(lines, "    ▸ "+styleSelected.Render(matches[i]+"/"))
		} else {
			lines = append(lines, "      "+matches[i]+"/")
		}
	}
	if n := len(matches) - start - browserRows; n > 0 {
		lines = append(lines, styleFaint.Render(fmt.Sprintf("      … %d more", n)))
	}
	return lines
}
