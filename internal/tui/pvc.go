package tui

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/mount"
)

// pvcPicker is the claim picker: the claims kubectl on a host can see,
// filtered as you type, to mount one here.
type pvcPicker struct {
	host     string
	context  string // asked for; "" for kubectl's current one
	loading  bool
	err      error
	res      *api.KubeListResult
	filter   textinput.Model
	cursor   int
	readOnly bool
	chosen   api.PVC // waiting for its mount point
}

type pvcListMsg struct {
	host, context string
	res           api.KubeListResult
	err           error
}

// WithPVCPicker opens the claim picker for host when the UI starts.
func (m Model) WithPVCPicker(host string) Model {
	m.openPVC(host)
	return m
}

// openPVC switches to the claim picker for host. It changes m; the caller
// must also run listPVCs.
func (m *Model) openPVC(host string) {
	f := textinput.New()
	f.Prompt, f.Placeholder = "filter> ", "type to filter by namespace, name or storage class"
	f.SetWidth(max(m.width-12, 10))
	f.Focus()
	m.mode = modePVC
	m.pvc = pvcPicker{host: host, loading: true, filter: f}
}

func (m Model) listPVCs() tea.Cmd {
	c, host, kctx := m.client, m.pvc.host, m.pvc.context
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		var res api.KubeListResult
		err := c.Call(ctx, api.MethodKubeList, api.KubeListParams{Host: host, Context: kctx}, &res)
		return pvcListMsg{host: host, context: kctx, res: res, err: err}
	}
}

func (m Model) gotPVCs(msg pvcListMsg) Model {
	if m.mode != modePVC && !m.choosingPath() || msg.host != m.pvc.host || msg.context != m.pvc.context {
		return m // stale
	}
	m.pvc.loading, m.pvc.err = false, msg.err
	if msg.err == nil {
		res := msg.res
		m.pvc.res = &res
		m.pvc.context = res.Context
	}
	m.pvc.cursor = 0
	return m
}

// choosingPath reports whether the mount point prompt for a picked claim is
// open.
func (m Model) choosingPath() bool { return m.mode == modeInput && m.inputKind == inputPVCPath }

// visiblePVCs are the listed claims matching the filter: every word typed
// must appear in the claim's namespace/name, storage class or status.
func (m Model) visiblePVCs() []api.PVC {
	if m.pvc.res == nil {
		return nil
	}
	terms := strings.Fields(strings.ToLower(m.pvc.filter.Value()))
	var out []api.PVC
	for _, p := range m.pvc.res.PVCs {
		hay := strings.ToLower(p.Ref() + " " + p.StorageClass + " " + p.Phase)
		if !slices.ContainsFunc(terms, func(t string) bool { return !strings.Contains(hay, t) }) {
			out = append(out, p)
		}
	}
	return out
}

func (m Model) pvcKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	visible := m.visiblePVCs()
	switch msg.String() {
	case "esc":
		if m.pvc.filter.Value() != "" {
			m.pvc.filter.Reset()
			m.pvc.cursor = 0
			return m, nil
		}
		m.mode = modeNormal
		return m, nil
	case "up", "ctrl+p":
		m.pvc.cursor = max(m.pvc.cursor-1, 0)
		return m, nil
	case "down", "ctrl+n":
		m.pvc.cursor = min(m.pvc.cursor+1, max(len(visible)-1, 0))
		return m, nil
	case "pgup":
		m.pvc.cursor = max(m.pvc.cursor-10, 0)
		return m, nil
	case "pgdown":
		m.pvc.cursor = min(m.pvc.cursor+10, max(len(visible)-1, 0))
		return m, nil
	case "ctrl+o":
		m.pvc.readOnly = !m.pvc.readOnly
		return m, nil
	case "ctrl+r":
		m.pvc.loading, m.pvc.err = true, nil
		return m, m.listPVCs()
	case "tab", "shift+tab":
		res := m.pvc.res
		if res == nil || len(res.Contexts) < 2 {
			return m, nil
		}
		step := 1
		if msg.String() == "shift+tab" {
			step = len(res.Contexts) - 1
		}
		i := slices.Index(res.Contexts, res.Context)
		m.pvc.context = res.Contexts[(i+step)%len(res.Contexts)]
		m.pvc.loading, m.pvc.err, m.pvc.res = true, nil, nil
		return m, m.listPVCs()
	case "enter":
		if m.pvc.cursor >= len(visible) {
			return m, nil
		}
		p := visible[m.pvc.cursor]
		if !p.Mountable {
			m.flash, m.flashErr = fmt.Sprintf("can't mount %s: %s", p.Ref(), p.Note), true
			return m, nil
		}
		m.pvc.chosen = p
		cmd := m.prompt(inputPVCPath, m.pvc.host)
		m.input.SetValue(m.defaultPVCPath(p))
		m.input.CursorEnd()
		return m, cmd
	}
	var cmd tea.Cmd
	before := m.pvc.filter.Value()
	m.pvc.filter, cmd = m.pvc.filter.Update(msg)
	if m.pvc.filter.Value() != before {
		m.pvc.cursor = 0
	}
	return m, cmd
}

// defaultPVCPath is where the daemon would mount p by default.
func (m Model) defaultPVCPath(p api.PVC) string {
	res := m.pvc.res
	return path.Join(res.MountRoot, strings.ReplaceAll(res.Context, "/", "_"), p.Namespace, p.Name)
}

// mountPVC mounts the chosen claim at the path typed in.
func (m Model) mountPVC(local string) (tea.Model, tea.Cmd) {
	m.mode = modeNormal
	p, res := m.pvc.chosen, m.pvc.res
	if !strings.HasPrefix(local, "~") && !filepath.IsAbs(local) {
		local, _ = filepath.Abs(local)
	}
	params := api.MountParams{
		Host: m.pvc.host, Direction: string(mount.PVCToLocal), Local: local,
		Kube: &api.KubeMount{Context: res.Context, Namespace: p.Namespace, PVC: p.Name, ReadOnly: m.pvc.readOnly},
	}
	if m.pvc.readOnly {
		params.Options = []string{"ro"}
	}
	return m, m.call(fmt.Sprintf("mounting %s at %s (starting a helper pod; see the log)", p.Ref(), local), api.MethodMountAdd, params)
}

// mountedPVC is the mount of a claim on the picker's host, if any.
func (m Model) mountedPVC(p api.PVC) (api.MountStatus, bool) {
	for _, mt := range m.hostStatus(m.pvc.host).Mounts {
		if k := mt.Kube; k != nil && k.Namespace == p.Namespace && k.PVC == p.Name && (k.Context == "" || k.Context == m.pvc.res.Context) {
			return mt, true
		}
	}
	return api.MountStatus{}, false
}

func (m Model) pvcView(height int) []string {
	title := "CLAIMS on " + m.pvc.host
	if res := m.pvc.res; res != nil {
		title += " · context " + res.Context
		if len(res.Contexts) > 1 {
			title += fmt.Sprintf(" (%d of %d, tab for next)", slices.Index(res.Contexts, res.Context)+1, len(res.Contexts))
		}
	}
	ro := "read-write"
	if m.pvc.readOnly {
		ro = styleWarn.Render("read-only")
	}
	lines := []string{styleSection.Render(title) + "  " + ro, m.pvc.filter.View(), ""}
	switch {
	case m.pvc.loading:
		return append(lines, "listing claims with kubectl on "+m.pvc.host+"…")
	case m.pvc.err != nil:
		return append(lines, styleErr.Render(m.pvc.err.Error()), "", styleFaint.Render("ctrl+r to retry · d on the host runs doctor"))
	}
	visible := m.visiblePVCs()
	if len(visible) == 0 {
		if len(m.pvc.res.PVCs) == 0 {
			return append(lines, "no claims in this context")
		}
		return append(lines, "nothing matches")
	}
	nameW := len("NAMESPACE/NAME")
	for _, p := range visible {
		nameW = max(nameW, min(len(p.Ref()), 60))
	}
	lines = append(lines, styleFaint.Render(fmt.Sprintf("    %-*s  %-8s %-7s %-14s %-8s %s", nameW, "NAMESPACE/NAME", "STATUS", "SIZE", "CLASS", "MODES", "USED BY")))

	rows := make([]string, len(visible))
	for i, p := range visible {
		mark := "  "
		if _, ok := m.mountedPVC(p); ok {
			mark = styleOK.Render("● ")
		}
		text := fmt.Sprintf("%-*s  %-8s %-7s %-14s %-8s %s", nameW, p.Ref(), p.Phase, dash(p.Size()), dash(p.StorageClass),
			dash(strings.Join(p.AccessModes, ",")), dash(usedBy(p)))
		switch {
		case i == m.pvc.cursor:
			text = styleSelected.Render(text)
		case !p.Mountable:
			text = styleFaint.Render(text)
		}
		marker := "  "
		if i == m.pvc.cursor {
			marker = "▸ "
		}
		rows[i] = marker + mark + text
	}
	avail := max(height-len(lines), 1)
	start := 0
	if m.pvc.cursor >= avail {
		start = m.pvc.cursor - avail + 1
	}
	return append(lines, rows[start:min(start+avail, len(rows))]...)
}

// pvcDetail describes the selected claim: whether and how it can be mounted.
func (m Model) pvcDetail() string {
	visible := m.visiblePVCs()
	if m.pvc.loading || m.pvc.err != nil || m.pvc.cursor >= len(visible) {
		return ""
	}
	p := visible[m.pvc.cursor]
	if mt, ok := m.mountedPVC(p); ok {
		return styleOK.Render(fmt.Sprintf("%s is mounted at %s (%s)", p.Ref(), mt.Local, mt.State))
	}
	if !p.Mountable {
		return styleErr.Render("can't mount: " + p.Note)
	}
	if p.Note != "" {
		return styleWarn.Render(p.Note)
	}
	return styleFaint.Render(p.Ref() + ": enter to mount")
}

func usedBy(p api.PVC) string {
	var pods []string
	for _, u := range p.UsedBy {
		pods = append(pods, u.Pod)
	}
	return strings.Join(pods, ",")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
