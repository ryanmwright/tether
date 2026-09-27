// Package tui is tether's terminal UI: a live view of hosts, forwards and
// profiles, with keys to change them.
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/mount"
	"github.com/ryanmwright/tether/internal/rpc"
)

// Client is the daemon connection. The caller must already have subscribed
// to status and log events on it. *rpc.Client implements it.
type Client interface {
	Call(ctx context.Context, method string, params, result any) error
	Notifications() <-chan rpc.Notification
}

const (
	maxLogs     = 200
	callTimeout = 60 * time.Second
)

type mode int

const (
	modeNormal mode = iota
	modeInput       // typing a forward spec
	modeDoctor
	modeHelp
)

type rowKind int

const (
	rowHost rowKind = iota
	rowForward
	rowMount
	rowProfile
)

// row is one selectable line. Its identity (kind, host, name) survives status
// updates, so the selection stays put as things change.
type row struct {
	kind rowKind
	host string
	name string // forward spec, mount key or profile name
}

type Model struct {
	client Client
	status api.Status
	have   bool // received a status yet
	err    error

	rows     []row
	cursor   int
	logs     []api.LogEntry
	showLogs bool
	width    int
	height   int

	mode       mode
	input      textinput.Model
	inputHost  string
	inputMount bool // the prompt is for a mount rather than a forward

	doctorHost    string
	doctor        *api.DoctorResult
	doctorErr     error
	doctorRunning bool

	flash    string
	flashErr bool
}

// New creates the model. logs seeds the log pane with recent history.
func New(c Client, logs []api.LogEntry) Model {
	in := textinput.New()
	if len(logs) > maxLogs {
		logs = logs[len(logs)-maxLogs:]
	}
	return Model{client: c, logs: logs, input: in, showLogs: true, width: 80, height: 24}
}

const (
	forwardPlaceholder = "L:8080:localhost:80   R:0:localhost:3000   D:1080   gpg-agent"
	mountPlaceholder   = "remote:~/src ~/mnt/src   or   ~/proj remote:~/proj"
)

type (
	statusMsg       api.Status
	logMsg          api.LogEntry
	disconnectedMsg struct{}
	tickMsg         time.Time
	actionMsg       struct {
		text string
		err  error
	}
	doctorMsg struct {
		host string
		res  api.DoctorResult
		err  error
	}
)

func (m Model) Init() tea.Cmd {
	return tea.Batch(waitEvent(m.client), tick())
}

// waitEvent delivers the next daemon event. Update re-issues it after each.
func waitEvent(c Client) tea.Cmd {
	return func() tea.Msg {
		for n := range c.Notifications() {
			switch n.Method {
			case api.EventStatus:
				var st api.Status
				if json.Unmarshal(n.Params, &st) == nil {
					return statusMsg(st)
				}
			case api.EventLog:
				var e api.LogEntry
				if json.Unmarshal(n.Params, &e) == nil {
					return logMsg(e)
				}
			}
		}
		return disconnectedMsg{}
	}
}

// tick redraws once a second so retry countdowns and uptime stay current.
func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.SetWidth(max(msg.Width-12, 10))
		return m, nil
	case statusMsg:
		m.setStatus(api.Status(msg))
		return m, waitEvent(m.client)
	case logMsg:
		m.logs = append(m.logs, api.LogEntry(msg))
		if len(m.logs) > maxLogs {
			m.logs = slices.Delete(m.logs, 0, len(m.logs)-maxLogs)
		}
		return m, waitEvent(m.client)
	case disconnectedMsg:
		m.err = fmt.Errorf("lost connection to the daemon")
		return m, nil
	case tickMsg:
		return m, tick()
	case actionMsg:
		m.flash, m.flashErr = msg.text, msg.err != nil
		if msg.err != nil {
			m.flash = msg.text + ": " + msg.err.Error()
		}
		return m, nil
	case doctorMsg:
		if msg.host == m.doctorHost {
			m.doctorRunning = false
			m.doctor, m.doctorErr = &msg.res, msg.err
		}
		return m, nil
	case tea.KeyPressMsg:
		return m.key(msg)
	}
	if m.mode == modeInput {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *Model) setStatus(st api.Status) {
	var selected row
	if m.cursor < len(m.rows) {
		selected = m.rows[m.cursor]
	}
	m.status, m.have = st, true
	m.rows = m.rows[:0]
	for _, h := range st.Hosts {
		m.rows = append(m.rows, row{kind: rowHost, host: h.Name})
		for _, f := range h.Forwards {
			m.rows = append(m.rows, row{kind: rowForward, host: h.Name, name: f.Spec})
		}
		for _, mt := range h.Mounts {
			m.rows = append(m.rows, row{kind: rowMount, host: h.Name, name: mt.Key})
		}
	}
	for _, p := range st.Profiles {
		m.rows = append(m.rows, row{kind: rowProfile, host: p.Host, name: p.Name})
	}
	if i := slices.Index(m.rows, selected); i >= 0 {
		m.cursor = i
	} else {
		m.cursor = min(m.cursor, max(len(m.rows)-1, 0))
	}
}

func (m Model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		return m, tea.Quit
	}
	switch m.mode {
	case modeHelp:
		m.mode = modeNormal
		return m, nil
	case modeDoctor:
		if k == "esc" || k == "q" || k == "d" {
			m.mode = modeNormal
		}
		return m, nil
	case modeInput:
		switch k {
		case "esc":
			m.mode = modeNormal
			m.input.Blur()
			return m, nil
		case "enter":
			value := strings.TrimSpace(m.input.Value())
			m.mode = modeNormal
			m.input.Blur()
			if value == "" {
				return m, nil
			}
			if m.inputMount {
				return m.addMount(value)
			}
			return m, m.call("added "+value+" on "+m.inputHost, api.MethodForwardAdd, api.ForwardParams{Host: m.inputHost, Spec: value})
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	m.flash = ""
	sel, ok := m.selected()
	switch k {
	case "q":
		return m, tea.Quit
	case "?":
		m.mode = modeHelp
	case "up", "k":
		m.cursor = max(m.cursor-1, 0)
	case "down", "j":
		m.cursor = min(m.cursor+1, max(len(m.rows)-1, 0))
	case "home":
		m.cursor = 0
	case "end":
		m.cursor = max(len(m.rows)-1, 0)
	case "l":
		m.showLogs = !m.showLogs
	case "r":
		return m, m.call("config reloaded", api.MethodReload, nil)
	case "enter", "space":
		if ok {
			return m.toggle(sel)
		}
	case "u":
		if ok {
			return m, m.up(sel)
		}
	case "x":
		if ok {
			return m.remove(sel)
		}
	case "a", "m":
		if ok {
			m.mode, m.inputHost, m.inputMount = modeInput, sel.host, k == "m"
			m.input.Reset()
			m.input.Prompt, m.input.Placeholder = "forward> ", forwardPlaceholder
			if m.inputMount {
				m.input.Prompt, m.input.Placeholder = "mount> ", mountPlaceholder
			}
			return m, m.input.Focus()
		}
	case "g":
		if ok {
			return m, m.toggleGPG(sel.host)
		}
	case "d":
		if ok {
			m.mode, m.doctorHost, m.doctor, m.doctorErr, m.doctorRunning = modeDoctor, sel.host, nil, nil, true
			return m, m.runDoctor(sel.host)
		}
	}
	return m, nil
}

func (m Model) selected() (row, bool) {
	if m.cursor < len(m.rows) {
		return m.rows[m.cursor], true
	}
	return row{}, false
}

// toggle brings the selection up or down, or removes an ad-hoc forward.
func (m Model) toggle(r row) (tea.Model, tea.Cmd) {
	switch r.kind {
	case rowHost:
		if m.hostStatus(r.host).State == api.StateDown {
			return m, m.up(r)
		}
		return m, m.down(r)
	case rowProfile:
		if m.profileStatus(r.name).Active {
			return m, m.down(r)
		}
		return m, m.up(r)
	default:
		return m.remove(r)
	}
}

func (m Model) up(r row) tea.Cmd {
	if r.kind == rowProfile {
		return m.call("activated "+r.name, api.MethodUp, api.TargetParams{Name: r.name, Kind: api.TargetProfile})
	}
	return m.call("connecting "+r.host, api.MethodUp, api.TargetParams{Name: r.host, Kind: api.TargetHost})
}

func (m Model) down(r row) tea.Cmd {
	if r.kind == rowProfile {
		return m.call("deactivated "+r.name, api.MethodDown, api.TargetParams{Name: r.name, Kind: api.TargetProfile})
	}
	return m.call("disconnected "+r.host, api.MethodDown, api.TargetParams{Name: r.host, Kind: api.TargetHost})
}

// remove takes down what the selection names: an ad-hoc forward or mount,
// a profile or a host.
func (m Model) remove(r row) (tea.Model, tea.Cmd) {
	if r.kind == rowMount {
		mt := m.mountStatus(r.host, r.name)
		if !mt.AdHoc {
			m.flash, m.flashErr = fmt.Sprintf("this mount comes from profile %s; deactivate the profile to remove it", strings.Join(mt.Profiles, ", ")), true
			return m, nil
		}
		return m, m.call("unmounted "+r.name, api.MethodMountRemove, api.MountParams{Host: r.host, Direction: mt.Direction, Remote: mt.Remote, Local: mt.Local})
	}
	if r.kind != rowForward {
		return m, m.down(r)
	}
	f := m.forwardStatus(r.host, r.name)
	if !f.AdHoc {
		m.flash, m.flashErr = fmt.Sprintf("%s comes from profile %s; deactivate the profile to remove it", r.name, strings.Join(f.Profiles, ", ")), true
		return m, nil
	}
	return m, m.call("removed "+r.name, api.MethodForwardRemove, api.ForwardParams{Host: r.host, Spec: r.name})
}

// addMount parses "SRC DST" from the prompt, one side marked remote:.
func (m Model) addMount(value string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(value)
	if len(fields) != 2 {
		m.flash, m.flashErr = "enter two paths: SRC DST, one of them remote:PATH", true
		return m, nil
	}
	spec, err := mount.Parse(fields[0], fields[1])
	if err != nil {
		m.flash, m.flashErr = err.Error(), true
		return m, nil
	}
	if !strings.HasPrefix(spec.Local, "~") && !filepath.IsAbs(spec.Local) {
		spec.Local, _ = filepath.Abs(spec.Local)
	}
	return m, m.call("mounting "+value+" on "+m.inputHost, api.MethodMountAdd,
		api.MountParams{Host: m.inputHost, Direction: string(spec.Direction), Remote: spec.Remote, Local: spec.Local})
}

func (m Model) toggleGPG(host string) tea.Cmd {
	if m.forwardStatus(host, "gpg-agent").AdHoc {
		return m.call("stopped gpg-agent on "+host, api.MethodForwardRemove, api.ForwardParams{Host: host, Spec: "gpg-agent"})
	}
	return m.call("forwarding gpg-agent to "+host, api.MethodForwardAdd, api.ForwardParams{Host: host, Spec: "gpg-agent"})
}

func (m Model) call(done, method string, params any) tea.Cmd {
	c := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		return actionMsg{text: done, err: c.Call(ctx, method, params, nil)}
	}
}

func (m Model) runDoctor(host string) tea.Cmd {
	c := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		var res api.DoctorResult
		err := c.Call(ctx, api.MethodDoctor, api.DoctorParams{Host: host}, &res)
		return doctorMsg{host: host, res: res, err: err}
	}
}

func (m Model) hostStatus(name string) api.HostStatus {
	for _, h := range m.status.Hosts {
		if h.Name == name {
			return h
		}
	}
	return api.HostStatus{Name: name}
}

func (m Model) profileStatus(name string) api.ProfileStatus {
	for _, p := range m.status.Profiles {
		if p.Name == name {
			return p
		}
	}
	return api.ProfileStatus{Name: name}
}

func (m Model) mountStatus(host, key string) api.MountStatus {
	for _, mt := range m.hostStatus(host).Mounts {
		if mt.Key == key {
			return mt
		}
	}
	return api.MountStatus{Key: key}
}

func (m Model) forwardStatus(host, spec string) api.ForwardStatus {
	for _, f := range m.hostStatus(host).Forwards {
		if f.Spec == spec {
			return f
		}
	}
	return api.ForwardStatus{Spec: spec}
}

// Styles. ANSI palette colors, so they follow the terminal's theme.
var (
	styleTitle    = lipgloss.NewStyle().Bold(true)
	styleFaint    = lipgloss.NewStyle().Faint(true)
	styleSection  = lipgloss.NewStyle().Bold(true).Faint(true)
	styleSelected = lipgloss.NewStyle().Reverse(true)
	styleErr      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleOK       = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn     = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

func stateStyle(s api.State) (lipgloss.Style, string) {
	switch s {
	case api.StateUp:
		return styleOK, "●"
	case api.StatePending:
		return styleWarn, "◌"
	case api.StateDegraded:
		return styleWarn, "◐"
	case api.StateError:
		return styleErr, "✕"
	default:
		return styleFaint, "○"
	}
}

func (m Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "tether"
	return v
}

func (m Model) render() string {
	if !m.have {
		if m.err != nil {
			return styleErr.Render(m.err.Error()) + "\n"
		}
		return "connecting to the daemon…\n"
	}
	top := []string{m.header()}
	if m.err != nil {
		top = append(top, styleErr.Render(m.err.Error()+" — press q to quit"))
	}
	if m.status.ConfigError != "" {
		first, _, _ := strings.Cut(m.status.ConfigError, "\n")
		top = append(top, styleErr.Render("config error (using last good config): "+first))
	}

	var bottom []string
	if m.mode == modeInput {
		what := "a forward to"
		if m.inputMount {
			what = "a mount on"
		}
		bottom = append(bottom, fmt.Sprintf("add %s %s (enter to add, esc to cancel)", what, m.inputHost), m.input.View())
	} else if detail := m.detail(); detail != "" {
		bottom = append(bottom, detail)
	}
	if m.flash != "" {
		style := styleOK
		if m.flashErr {
			style = styleErr
		}
		bottom = append(bottom, style.Render(m.flash))
	}
	logLines := 0
	if m.showLogs && m.mode == modeNormal {
		logLines = min(8, max(m.height/4, 3))
		bottom = append(bottom, m.logPane(logLines)...)
	}
	bottom = append(bottom, styleFaint.Render(m.footer()))

	bodyHeight := max(m.height-len(top)-len(bottom)-1, 3)
	var body []string
	switch m.mode {
	case modeDoctor:
		body = m.doctorView()
	case modeHelp:
		body = helpLines
	default:
		body = m.rowLines(bodyHeight)
	}
	if len(body) > bodyHeight {
		body = body[:bodyHeight]
	}
	for len(body) < bodyHeight {
		body = append(body, "")
	}

	lines := slices.Concat(top, []string{""}, body, bottom)
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, m.width, "…")
	}
	return strings.Join(lines, "\n")
}

func (m Model) header() string {
	st := m.status
	return styleTitle.Render("tether") + styleFaint.Render(fmt.Sprintf("  %s · pid %d · up %s · %s",
		st.Version, st.PID, time.Since(st.StartedAt).Round(time.Second), st.ConfigPath))
}

func (m Model) footer() string {
	switch m.mode {
	case modeDoctor:
		return "esc back"
	case modeHelp:
		return "any key to go back"
	}
	return "enter toggle · a forward · m mount · g gpg · d doctor · r reload · l log · ? help · q quit"
}

// rowLines renders the host/forward/profile list, scrolled to keep the
// cursor in view.
func (m Model) rowLines(height int) []string {
	if len(m.status.Hosts) == 0 {
		return []string{"no hosts configured — add some to " + m.status.ConfigPath + " and press r"}
	}
	nameW := 0
	for _, r := range m.rows {
		w := len(r.name)
		if r.kind == rowHost {
			w = len(r.host)
		}
		nameW = max(nameW, w+2)
	}

	var lines []string
	cursorLine := 0
	section := rowKind(-1)
	for i, r := range m.rows {
		if (r.kind == rowProfile) != (section == rowProfile) || section == -1 {
			if section != -1 {
				lines = append(lines, "")
			}
			title := "HOSTS"
			if r.kind == rowProfile {
				title = "PROFILES"
			}
			lines = append(lines, styleSection.Render(title))
			section = r.kind
		}
		if i == m.cursor {
			cursorLine = len(lines)
		}
		lines = append(lines, m.rowLine(r, i == m.cursor, nameW))
	}

	// Scroll so the cursor stays visible.
	start := 0
	if cursorLine >= height {
		start = cursorLine - height + 1
	}
	return lines[start:min(start+height, len(lines))]
}

func (m Model) rowLine(r row, selected bool, nameW int) string {
	var state api.State
	var name, info, extra string
	switch r.kind {
	case rowHost:
		h := m.hostStatus(r.host)
		state, name, info = h.State, h.Name, "ssh "+h.SSH
		if h.Autoconnect {
			info += " · auto"
		}
		extra = hostProblem(h)
	case rowForward:
		f := m.forwardStatus(r.host, r.name)
		state, name, info = f.State, "  "+f.Spec, source(f.Profiles, f.AdHoc)
		extra = forwardDetail(f)
	case rowMount:
		mt := m.mountStatus(r.host, r.name)
		state, name, info = mt.State, "  "+mt.Key, source(mt.Profiles, mt.AdHoc)
		extra = mt.Error
	case rowProfile:
		p := m.profileStatus(r.name)
		state, name, info = p.State, p.Name, "on "+p.Host
		if p.Autoconnect {
			info += " · auto"
		}
		extra = p.Error
	}
	style, dot := stateStyle(state)
	marker := "  "
	if selected {
		marker = "▸ "
	}
	text := fmt.Sprintf("%-*s %-9s %s", nameW, name, state, info)
	if selected {
		text = styleSelected.Render(text)
	}
	line := marker + style.Render(dot) + " " + text
	if extra != "" {
		line += "  " + styleFaint.Render(extra)
	}
	return line
}

func hostProblem(h api.HostStatus) string {
	if h.Error == "" {
		return ""
	}
	if h.RetryAt != nil {
		return fmt.Sprintf("%s (retry in %s)", h.Error, max(time.Until(*h.RetryAt).Round(time.Second), 0))
	}
	return h.Error
}

// source says where a forward or mount comes from.
func source(profiles []string, adhoc bool) string {
	sources := slices.Clip(profiles)
	if adhoc {
		sources = append(sources, "ad-hoc")
	}
	return strings.Join(sources, ",")
}

func forwardDetail(f api.ForwardStatus) string {
	switch {
	case f.Error != "":
		return f.Error
	case f.AllocatedPort != 0:
		return fmt.Sprintf("remote port %d", f.AllocatedPort)
	case f.Resolved != "":
		return f.Resolved
	}
	return ""
}

// detail describes the selection in full, for errors too long for its row.
func (m Model) detail() string {
	r, ok := m.selected()
	if !ok || m.mode != modeNormal {
		return ""
	}
	switch r.kind {
	case rowHost:
		if p := hostProblem(m.hostStatus(r.host)); p != "" {
			return styleErr.Render(r.host + ": " + p)
		}
	case rowForward:
		f := m.forwardStatus(r.host, r.name)
		if f.Error != "" {
			return styleErr.Render(f.Spec + ": " + f.Error)
		}
		if f.Resolved != "" {
			return styleFaint.Render(f.Spec + " → " + f.Resolved)
		}
	case rowMount:
		if e := m.mountStatus(r.host, r.name).Error; e != "" {
			return styleErr.Render(r.name + ": " + e)
		}
	case rowProfile:
		if e := m.profileStatus(r.name).Error; e != "" {
			return styleErr.Render(r.name + ": " + e)
		}
	}
	return ""
}

func (m Model) logPane(n int) []string {
	lines := []string{styleSection.Render("LOG")}
	logs := m.logs[max(len(m.logs)-n, 0):]
	for _, e := range logs {
		line := e.Line()
		switch e.Level {
		case "ERROR":
			line = styleErr.Render(line)
		case "WARN":
			line = styleWarn.Render(line)
		default:
			line = styleFaint.Render(line)
		}
		lines = append(lines, line)
	}
	for len(lines) < n+1 {
		lines = append(lines, "")
	}
	return lines
}

func (m Model) doctorView() []string {
	lines := []string{styleSection.Render("DOCTOR " + m.doctorHost)}
	switch {
	case m.doctorRunning:
		return append(lines, "checking…")
	case m.doctorErr != nil:
		return append(lines, styleErr.Render(m.doctorErr.Error()))
	}
	section := ""
	for _, c := range m.doctor.Checks {
		if c.Section != section {
			section = c.Section
			lines = append(lines, "", section)
		}
		mark := map[api.CheckStatus]string{
			api.CheckOK: styleOK.Render("ok  "), api.CheckWarn: styleWarn.Render("WARN"),
			api.CheckFail: styleErr.Render("FAIL"), api.CheckSkip: styleFaint.Render("skip"),
		}[c.Status]
		lines = append(lines, fmt.Sprintf("  %s  %-16s %s", mark, c.Name, c.Detail))
		if c.Fix != "" {
			lines = append(lines, styleFaint.Render("                          fix: "+c.Fix))
		}
	}
	return lines
}

var helpLines = []string{
	styleSection.Render("KEYS"),
	"  ↑/k ↓/j      move",
	"  enter/space  toggle: connect/disconnect a host, activate/deactivate a profile,",
	"               remove an ad-hoc forward or mount",
	"  u            bring up the selection now (also retries a failed connection)",
	"  x            take down the selection",
	"  a            add an ad-hoc forward to the selected host",
	"  m            add an ad-hoc mount on the selected host (SRC DST, one side remote:PATH)",
	"  g            toggle ad-hoc gpg-agent forwarding to the selected host",
	"  d            run doctor on the selected host",
	"  r            reload the config file",
	"  l            show/hide the log",
	"  q            quit (connections keep running in the daemon)",
	"",
	styleSection.Render("STATES"),
	"  " + styleOK.Render("●") + " up   " + styleWarn.Render("◌") + " pending   " + styleWarn.Render("◐") + " degraded   " +
		styleErr.Render("✕") + " error   " + styleFaint.Render("○") + " down",
}
