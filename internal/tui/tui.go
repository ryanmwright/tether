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
	rowUSB // a device shared with a host
	rowProfile
	rowUSBDevice // a device plugged in here
)

// row is one selectable line. Its identity (kind, host, name) survives status
// updates, so the selection stays put as things change.
type row struct {
	kind rowKind
	host string
	name string // forward spec, mount key, USB device, profile name or bus ID
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

	mode      mode
	input     textinput.Model
	inputHost string
	inputKind inputKind
	inputUSB  api.USBDevice // the device to share, for inputShare

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

// inputKind is what the prompt is asking for.
type inputKind int

const (
	inputForward inputKind = iota
	inputMount
	inputConnect
	inputShare
)

var prompts = map[inputKind]struct{ prompt, placeholder, title string }{
	inputForward: {"forward> ", "L:8080:localhost:80   R:0:localhost:3000   D:1080   gpg-agent", "add a forward to %s"},
	inputMount:   {"mount> ", "remote:~/src ~/mnt/src   or   ~/proj remote:~/proj", "add a mount on %s"},
	inputConnect: {"connect> ", "NAME [SSH-DEST]   e.g. devbox2   or   scratch me@10.0.0.5", "connect to a host that isn't in the config"},
	inputShare:   {"host> ", "HOST", "share %s with which host?"},
}

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
		for _, u := range h.USB {
			m.rows = append(m.rows, row{kind: rowUSB, host: h.Name, name: u.Device})
		}
	}
	for _, p := range st.Profiles {
		m.rows = append(m.rows, row{kind: rowProfile, host: p.Host, name: p.Name})
	}
	for _, d := range st.USB {
		m.rows = append(m.rows, row{kind: rowUSBDevice, name: d.BusID})
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
			switch m.inputKind {
			case inputMount:
				return m.addMount(value)
			case inputConnect:
				return m.addHost(value)
			case inputShare:
				return m, m.share(m.inputUSB, value)
			}
			return m, m.call("added "+value+" on "+m.inputHost, api.MethodForwardAdd, api.ForwardParams{Host: m.inputHost, Spec: value})
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	m.flash = ""
	sel, ok := m.selected()
	// Host keys act on the selection's host; local USB devices have none.
	hostOK := ok && sel.kind != rowUSBDevice
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
		if hostOK {
			return m, m.up(sel)
		}
	case "x":
		if ok {
			return m.remove(sel)
		}
	case "a", "m":
		if hostOK {
			kind := inputForward
			if k == "m" {
				kind = inputMount
			}
			cmd := m.prompt(kind, sel.host)
			return m, cmd
		}
	case "c":
		cmd := m.prompt(inputConnect, "")
		return m, cmd
	case "g":
		if hostOK {
			return m, m.toggleGPG(sel.host)
		}
	case "d":
		if hostOK {
			m.mode, m.doctorHost, m.doctor, m.doctorErr, m.doctorRunning = modeDoctor, sel.host, nil, nil, true
			return m, m.runDoctor(sel.host)
		}
	}
	return m, nil
}

// prompt opens the input line. It changes m, so call it before returning m
// (not in the same return statement: the evaluation order isn't defined).
func (m *Model) prompt(kind inputKind, host string) tea.Cmd {
	m.mode, m.inputKind, m.inputHost = modeInput, kind, host
	m.input.Reset()
	m.input.Prompt, m.input.Placeholder = prompts[kind].prompt, prompts[kind].placeholder
	return m.input.Focus()
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
	case rowUSBDevice:
		return m.toggleUSB(r.name)
	default:
		return m.remove(r)
	}
}

// toggleUSB stops sharing a local device, or shares it: with the only
// connected host, or one the user names.
func (m Model) toggleUSB(busid string) (tea.Model, tea.Cmd) {
	d := m.usbDevice(busid)
	if d.Host != "" {
		u := m.sharedUSB(d)
		if !u.AdHoc {
			m.flash, m.flashErr = fmt.Sprintf("%s is shared by profile %s; deactivate the profile to stop sharing it", d.Title(), strings.Join(u.Profiles, ", ")), true
			return m, nil
		}
		return m, m.call("stopped sharing "+d.Title(), api.MethodUSBDetach, api.USBParams{Host: d.Host, Device: u.Device})
	}
	var connected []string
	for _, h := range m.status.Hosts {
		if h.State == api.StateUp || h.State == api.StateDegraded {
			connected = append(connected, h.Name)
		}
	}
	if len(connected) == 1 {
		return m, m.share(d, connected[0])
	}
	m.inputUSB = d
	cmd := m.prompt(inputShare, "")
	return m, cmd
}

func (m Model) share(d api.USBDevice, host string) tea.Cmd {
	return m.call("sharing "+d.Title()+" with "+host, api.MethodUSBAttach, api.USBParams{Host: host, Device: d.BusID})
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
	switch r.kind {
	case rowUSBDevice:
		if m.usbDevice(r.name).Host == "" {
			return m, nil
		}
		return m.toggleUSB(r.name)
	case rowUSB:
		u := m.usbStatus(r.host, r.name)
		if !u.AdHoc {
			m.flash, m.flashErr = fmt.Sprintf("USB %s comes from profile %s; deactivate the profile to stop sharing it", r.name, strings.Join(u.Profiles, ", ")), true
			return m, nil
		}
		return m, m.call("stopped sharing "+r.name, api.MethodUSBDetach, api.USBParams{Host: r.host, Device: r.name})
	}
	if r.kind == rowMount {
		mt := m.mountStatus(r.host, r.name)
		if !mt.AdHoc {
			m.flash, m.flashErr = fmt.Sprintf("this mount comes from profile %s; deactivate the profile to remove it", strings.Join(mt.Profiles, ", ")), true
			return m, nil
		}
		return m, m.call("unmounted "+r.name, api.MethodMountRemove, api.MountParams{Host: r.host, Direction: mt.Direction, Remote: mt.Remote, Local: mt.Local})
	}
	if r.kind == rowHost {
		// An ad-hoc host that's already down is forgotten.
		if h := m.hostStatus(r.host); h.AdHoc && h.State == api.StateDown {
			return m, m.call("removed "+r.host, api.MethodHostRemove, api.HostParams{Name: r.host})
		}
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

// addHost parses "NAME [SSH-DEST]" from the prompt and connects to it.
func (m Model) addHost(value string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(value)
	if len(fields) > 2 {
		m.flash, m.flashErr = "enter NAME, or NAME SSH-DEST", true
		return m, nil
	}
	p := api.HostParams{Name: fields[0]}
	if len(fields) == 2 {
		p.SSH = fields[1]
	}
	return m, m.call("connecting "+p.Name, api.MethodHostAdd, p)
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

func (m Model) usbStatus(host, device string) api.USBStatus {
	for _, u := range m.hostStatus(host).USB {
		if u.Device == device {
			return u
		}
	}
	return api.USBStatus{Device: device}
}

func (m Model) usbDevice(busid string) api.USBDevice {
	for _, d := range m.status.USB {
		if d.BusID == busid {
			return d
		}
	}
	return api.USBDevice{BusID: busid}
}

// sharedUSB is the host's entry for a local device that is shared.
func (m Model) sharedUSB(d api.USBDevice) api.USBStatus {
	for _, u := range m.hostStatus(d.Host).USB {
		if u.BusID == d.BusID {
			return u
		}
	}
	return api.USBStatus{}
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
		title := prompts[m.inputKind].title
		switch {
		case m.inputKind == inputShare:
			title = fmt.Sprintf(title, m.inputUSB.Title())
		case strings.Contains(title, "%s"):
			title = fmt.Sprintf(title, m.inputHost)
		}
		bottom = append(bottom, title+" (enter to confirm, esc to cancel)", m.input.View())
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
	return m.enterHint() + " · c connect to… · a forward · m mount · g gpg · d doctor · ? help · q quit"
}

// enterHint says what enter does on the selected row.
func (m Model) enterHint() string {
	r, ok := m.selected()
	if !ok {
		return "enter —"
	}
	switch r.kind {
	case rowHost:
		if m.hostStatus(r.host).State == api.StateDown {
			return "enter connect"
		}
		return "enter disconnect"
	case rowProfile:
		if m.profileStatus(r.name).Active {
			return "enter deactivate"
		}
		return "enter activate"
	case rowForward:
		if m.forwardStatus(r.host, r.name).AdHoc {
			return "enter remove"
		}
	case rowMount:
		if m.mountStatus(r.host, r.name).AdHoc {
			return "enter unmount"
		}
	case rowUSB:
		if m.usbStatus(r.host, r.name).AdHoc {
			return "enter stop sharing"
		}
	case rowUSBDevice:
		d := m.usbDevice(r.name)
		if d.Host == "" {
			return "enter share"
		}
		if m.sharedUSB(d).AdHoc {
			return "enter stop sharing"
		}
	}
	return "enter —"
}

// rowLines renders the host/forward/profile list, scrolled to keep the
// cursor in view.
func (m Model) rowLines(height int) []string {
	if len(m.status.Hosts) == 0 {
		return []string{"no hosts yet — press c to connect to one, or add hosts to " + m.status.ConfigPath + " and press r"}
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
	section := ""
	for i, r := range m.rows {
		if title := sectionTitle(r.kind); title != section {
			if section != "" {
				lines = append(lines, "")
			}
			lines = append(lines, styleSection.Render(title))
			if r.kind == rowUSBDevice && m.status.USBUnavailable != "" {
				lines = append(lines, "  "+styleWarn.Render("⚠ "+m.status.USBUnavailable))
			}
			section = title
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

func sectionTitle(k rowKind) string {
	switch k {
	case rowProfile:
		return "PROFILES"
	case rowUSBDevice:
		return "USB DEVICES"
	}
	return "HOSTS"
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
		if h.AdHoc {
			info += " · ad-hoc"
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
	case rowUSB:
		u := m.usbStatus(r.host, r.name)
		state, name, info = u.State, "  usb "+u.Device, source(u.Profiles, u.AdHoc)
		extra = u.Error
		if extra == "" && u.Name != "" {
			extra = u.Name
		}
	case rowUSBDevice:
		d := m.usbDevice(r.name)
		state, name, info = api.StateDown, d.BusID, d.Title()
		if d.Host != "" {
			state = m.sharedUSB(d).State
			info += " · on " + d.Host
		}
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
	case rowUSB:
		if e := m.usbStatus(r.host, r.name).Error; e != "" {
			return styleErr.Render("USB " + r.name + ": " + e)
		}
	case rowUSBDevice:
		if d := m.usbDevice(r.name); d.Host != "" {
			if e := m.sharedUSB(d).Error; e != "" {
				return styleErr.Render(d.Title() + ": " + e)
			}
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
	"               remove an ad-hoc forward or mount; on a USB device, share it with a",
	"               host (the connected one, or asks which) or stop sharing it",
	"  u            bring up the selection now (also retries a failed connection)",
	"  x            take down the selection; on an ad-hoc host that's down, forget it",
	"  c            connect to a host that isn't in the config (NAME [SSH-DEST])",
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
