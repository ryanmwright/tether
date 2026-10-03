package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/forward"
	"github.com/ryanmwright/tether/internal/mount"
)

// addEntry is one thing the Add menu offers for a host.
type addEntry struct {
	key, title, help string
	local            bool // offered on the local host (which only has Kubernetes items)
	open             func(m *Model, host string) tea.Cmd
}

var addEntries = []addEntry{
	{key: "l", title: "Forward a local port to a port on HOST", help: "e.g. reach a database running on HOST", open: openFwdForm("local")},
	{key: "r", title: "Forward a port on HOST to a port here", help: "e.g. let HOST reach a dev server running here", open: openFwdForm("remote")},
	{key: "n", title: "Forward a local port to a machine on HOST's network", help: "HOST as a jump box: reach db.internal:5432 from here", open: openFwdForm("tohost")},
	{key: "b", title: "Forward a port on HOST to a machine on my network", help: "let HOST reach e.g. nas.lan:9000 through here", open: openFwdForm("fromlan")},
	{key: "s", title: "SOCKS proxy here, connecting out from HOST", help: "browse or curl as if you were on HOST", open: openFwdForm("socks")},
	{key: "v", title: "SOCKS proxy on HOST, connecting out from here", help: "give HOST your network or internet access", open: openFwdForm("rsocks")},
	{key: "h", title: "HTTP proxy here, connecting out from HOST", help: "for tools that only take HTTP_PROXY/HTTPS_PROXY", open: openFwdForm("http")},
	{key: "k", title: "Kubernetes service or pod…", help: "kubectl port-forward on HOST, brought here", local: true, open: openKubeFwd},
	{key: "d", title: "Mount a directory from HOST here", help: "sshfs", open: openMountForm(false)},
	{key: "t", title: "Mount a local directory on HOST", help: "HOST sees only that directory", open: openMountForm(true)},
	{key: "p", title: "Mount a Kubernetes claim (PVC)…", help: "a helper pod serves it through kubectl", local: true, open: func(m *Model, host string) tea.Cmd {
		m.openPVC(host)
		return m.listPVCs()
	}},
	{key: "u", title: "Share a USB device with HOST", help: "USB/IP: a security key, a board, a serial adapter", open: openUSBForm},
	{key: "g", title: "Forward gpg-agent to HOST (toggle)", help: "sign and decrypt on HOST with your keys", open: func(m *Model, host string) tea.Cmd {
		m.mode = modeNormal
		return m.toggleGPG(host)
	}},
	{key: "x", title: "Type a forward spec…", help: "any L:, R:, D:, H: or K: spec, shorthand or name", local: true, open: func(m *Model, host string) tea.Cmd {
		return m.prompt(inputForward, host)
	}},
}

// addMenu is the Add menu for a host.
type addMenu struct {
	host    string
	entries []addEntry
	cursor  int
}

func (m *Model) openAdd(host string) {
	var entries []addEntry
	local := m.hostStatus(host).Local
	for _, e := range addEntries {
		if !local || e.local {
			entries = append(entries, e)
		}
	}
	m.mode, m.add = modeAdd, addMenu{host: host, entries: entries}
}

func (m Model) addKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	a := &m.add
	switch k := msg.String(); k {
	case "esc", "q":
		m.mode = modeNormal
	case "up":
		a.cursor = max(a.cursor-1, 0)
	case "down":
		a.cursor = min(a.cursor+1, len(a.entries)-1)
	case "enter", "space":
		cmd := a.entries[a.cursor].open(&m, a.host)
		return m, cmd
	default:
		for _, e := range a.entries {
			if e.key == k {
				cmd := e.open(&m, a.host)
				return m, cmd
			}
		}
	}
	return m, nil
}

func (m Model) addView() []string {
	a := m.add
	lines := []string{styleSection.Render("ADD TO " + a.host), ""}
	for i, e := range a.entries {
		title := strings.ReplaceAll(e.title, "HOST", a.host)
		text := fmt.Sprintf("%s  %-60s", e.key, title)
		if i == a.cursor {
			lines = append(lines, "▸ "+styleSelected.Render(text)+"  "+styleFaint.Render(strings.ReplaceAll(e.help, "HOST", a.host)))
		} else {
			lines = append(lines, "  "+text)
		}
	}
	return lines
}

// openForm shows f, focused on its first field.
func (m *Model) openForm(f form) tea.Cmd {
	m.mode, m.form = modeForm, f
	return tea.Batch(m.form.setFocus(0), m.syncBrowser())
}

func (m Model) formKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" {
		m.mode = modeAdd
		if m.form.fromPicker {
			m.mode = modeKube
		}
		return m, nil
	}
	cmd, submit := m.form.key(msg)
	if !submit {
		return m, cmd
	}
	res, err := m.form.build(m.form.values())
	if err != nil {
		m.flash, m.flashErr = err.Error(), true
		return m, nil
	}
	m.mode = modeNormal
	return m, m.call(res.done, res.method, res.params)
}

var listenChoices = []string{"localhost only", "all interfaces (others can connect)"}

// bindPrefix is the listen address for a listen choice: "" or "*:".
func bindPrefix(choice string) string {
	if choice == listenChoices[1] {
		return "*:"
	}
	return ""
}

// forwardResult is what submitting a forward form does.
func forwardResult(host, label, spec string) (formResult, error) {
	s, err := forward.Parse(spec)
	if err != nil {
		return formResult{}, err
	}
	in := s.String()
	if label != "" {
		if _, _, err := forward.SplitLabel(label + "=x"); err != nil {
			return formResult{}, err
		}
		in = label + "=" + in
	}
	return formResult{
		preview: s.String() + " — " + forward.Describe(s, host),
		done:    "added " + in + " on " + host,
		method:  api.MethodForwardAdd,
		params:  api.ForwardParams{Host: host, Spec: in},
	}, nil
}

const labelHelp = "optional name, e.g. postgres"

// openFwdForm opens the form for one kind of forward.
func openFwdForm(kind string) func(m *Model, host string) tea.Cmd {
	return func(m *Model, host string) tea.Cmd {
		var f form
		switch kind {
		case "local":
			f = form{title: "FORWARD A LOCAL PORT TO A PORT ON " + host, fields: []formField{
				textField("Port on "+host, "e.g. 5432", "", "what's listening there"),
				textField("Local port", "same", "", "empty: the same number; 0: any free one"),
				choiceField("Listen on", "←/→", listenChoices...),
				textField("Label", "", "", labelHelp),
			}}
			f.build = func(v formValues) (formResult, error) {
				rp, err := portValue(v, 0, "", "the port on "+host)
				if err != nil {
					return formResult{}, err
				}
				lp, err := portValue(v, 1, rp, "the local port")
				if err != nil {
					return formResult{}, err
				}
				return forwardResult(host, v.get(3, ""), "L:"+bindPrefix(v[2])+lp+":localhost:"+rp)
			}
		case "remote":
			f = form{title: "FORWARD A PORT ON " + host + " TO A PORT HERE", fields: []formField{
				textField("Port here", "e.g. 3000", "", "what's listening on this machine"),
				textField("Port on "+host, "same", "", "empty: the same number; 0: one the server picks"),
				textField("Label", "", "", labelHelp),
			}}
			f.build = func(v formValues) (formResult, error) {
				lp, err := portValue(v, 0, "", "the port here")
				if err != nil {
					return formResult{}, err
				}
				rp, err := portValue(v, 1, lp, "the port on "+host)
				if err != nil {
					return formResult{}, err
				}
				return forwardResult(host, v.get(2, ""), "R:"+rp+":localhost:"+lp)
			}
		case "tohost":
			f = form{title: "FORWARD A LOCAL PORT TO A MACHINE ON " + host + "'S NETWORK", fields: []formField{
				textField("Machine", "e.g. db.internal", "", "its name or address, as "+host+" sees it"),
				textField("Its port", "e.g. 5432", "", ""),
				textField("Local port", "same", "", "empty: the same number; 0: any free one"),
				choiceField("Listen on", "←/→", listenChoices...),
				textField("Label", "", "", labelHelp),
			}}
			f.build = func(v formValues) (formResult, error) {
				target := v.get(0, "")
				if target == "" {
					return formResult{}, errors.New("enter the machine's name or address")
				}
				tp, err := portValue(v, 1, "", "its port")
				if err != nil {
					return formResult{}, err
				}
				lp, err := portValue(v, 2, tp, "the local port")
				if err != nil {
					return formResult{}, err
				}
				return forwardResult(host, v.get(4, ""), "L:"+bindPrefix(v[3])+lp+":"+bracket(target)+":"+tp)
			}
		case "fromlan":
			f = form{title: "FORWARD A PORT ON " + host + " TO A MACHINE ON MY NETWORK", fields: []formField{
				textField("Machine", "e.g. nas.lan", "", "its name or address, as this machine sees it"),
				textField("Its port", "e.g. 9000", "", ""),
				textField("Port on "+host, "same", "", "empty: the same number"),
				textField("Label", "", "", labelHelp),
			}}
			f.build = func(v formValues) (formResult, error) {
				target := v.get(0, "")
				if target == "" {
					return formResult{}, errors.New("enter the machine's name or address")
				}
				tp, err := portValue(v, 1, "", "its port")
				if err != nil {
					return formResult{}, err
				}
				rp, err := portValue(v, 2, tp, "the port on "+host)
				if err != nil {
					return formResult{}, err
				}
				return forwardResult(host, v.get(3, ""), "R:"+rp+":"+bracket(target)+":"+tp)
			}
		case "socks", "http":
			kindLetter, def, what := "D", 1080, "SOCKS PROXY"
			if kind == "http" {
				kindLetter, def, what = "H", 8080, "HTTP PROXY"
			}
			f = form{title: what + " HERE, CONNECTING OUT FROM " + host, fields: []formField{
				textField("Local port", "", strconv.Itoa(freePort(def)), "0: any free one"),
				choiceField("Listen on", "←/→", listenChoices...),
				textField("Label", "", "", labelHelp),
			}}
			f.build = func(v formValues) (formResult, error) {
				lp, err := portValue(v, 0, "", "the local port")
				if err != nil {
					return formResult{}, err
				}
				return forwardResult(host, v.get(2, ""), kindLetter+":"+bindPrefix(v[1])+lp)
			}
		case "rsocks":
			f = form{title: "SOCKS PROXY ON " + host + ", CONNECTING OUT FROM HERE", fields: []formField{
				textField("Port on "+host, "", "1080", "programs on "+host+" use localhost:PORT as their SOCKS proxy"),
				textField("Label", "", "", labelHelp),
			}}
			f.build = func(v formValues) (formResult, error) {
				rp, err := portValue(v, 0, "", "the port on "+host)
				if err != nil {
					return formResult{}, err
				}
				return forwardResult(host, v.get(1, ""), "R:"+rp)
			}
		}
		return m.openForm(f)
	}
}

func bracket(host string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]"
	}
	return host
}

// openMountForm opens the form for mounting a directory either way.
func openMountForm(there bool) func(m *Model, host string) tea.Cmd {
	return func(m *Model, host string) tea.Cmd {
		home, _ := os.UserHomeDir()
		var f form
		if there {
			cwd, _ := os.Getwd()
			f = form{title: "MOUNT A LOCAL DIRECTORY ON " + host, fields: []formField{
				pathField("Local directory", "", cwd+"/", "must exist; "+host+" sees only this", false, ""),
				pathField("Mount on "+host+" at", "~/<its name>", "", "relative to "+host+"'s home unless absolute", true, host),
			}}
			f.build = func(v formValues) (formResult, error) {
				local := v.get(0, "")
				if local == "" {
					return formResult{}, errors.New("enter the local directory")
				}
				local = strings.TrimSuffix(absLocal(local, home), "/")
				remote := strings.TrimSuffix(v.get(1, "~/"+filepath.Base(local)), "/")
				return mountResult(host, api.MountParams{Host: host, Direction: string(mount.LocalToRemote), Local: local, Remote: remote})
			}
		} else {
			f = form{title: "MOUNT A DIRECTORY FROM " + host + " HERE", fields: []formField{
				pathField("Directory on "+host, "", "~/", "relative to "+host+"'s home unless absolute", true, host),
				pathField("Mount here at", "~/mnt/<its name>", "", "created if missing", false, ""),
			}}
			f.build = func(v formValues) (formResult, error) {
				remote := v.get(0, "")
				if remote == "" {
					return formResult{}, errors.New("enter the directory on " + host)
				}
				if remote != "/" && remote != "~/" {
					remote = strings.TrimSuffix(remote, "/")
				}
				base := path.Base(strings.TrimRight(remote, "/"))
				if base == "~" || base == "." || base == "/" {
					base = host
				}
				local := strings.TrimSuffix(absLocal(v.get(1, "~/mnt/"+base), home), "/")
				return mountResult(host, api.MountParams{Host: host, Direction: string(mount.RemoteToLocal), Local: local, Remote: remote})
			}
		}
		return m.openForm(f)
	}
}

// absLocal makes a local path absolute: ~/ is kept for the daemon, other
// relative paths are from the current directory.
func absLocal(p, home string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || filepath.IsAbs(p) {
		return p
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Join(home, p)
	}
	return abs
}

func mountResult(host string, p api.MountParams) (formResult, error) {
	src, dst := mount.RemotePrefix+p.Remote, p.Local
	if p.Direction == string(mount.LocalToRemote) {
		src, dst = p.Local, mount.RemotePrefix+p.Remote
	}
	spec, err := mount.Parse(src, dst)
	if err != nil {
		return formResult{}, err
	}
	if _, err := mount.Normalize(spec, "/home"); err != nil {
		return formResult{}, err
	}
	return formResult{
		preview: src + " → " + dst,
		done:    "mounting " + src + " at " + dst + " on " + host,
		method:  api.MethodMountAdd,
		params:  p,
	}, nil
}

// usbPicker lists the USB devices plugged in here, to share one with host.
type usbPicker struct {
	host   string
	cursor int
}

// openUSBForm shows every USB device here, to share one with host.
func openUSBForm(m *Model, host string) tea.Cmd {
	if len(m.status.USB) == 0 {
		m.mode = modeNormal
		m.flash, m.flashErr = "no USB devices here to share", true
		if m.status.USBUnavailable != "" {
			m.flash += ": " + m.status.USBUnavailable
		}
		return nil
	}
	m.mode, m.usb = modeUSB, usbPicker{host: host}
	// Start on the first device that's free.
	if i := slices.IndexFunc(m.status.USB, func(d api.USBDevice) bool { return d.Host == "" }); i >= 0 {
		m.usb.cursor = i
	}
	return nil
}

func (m Model) usbKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	devs := m.status.USB
	switch msg.String() {
	case "esc", "q":
		m.mode = modeAdd
	case "up", "k":
		m.usb.cursor = max(m.usb.cursor-1, 0)
	case "down", "j":
		m.usb.cursor = min(m.usb.cursor+1, max(len(devs)-1, 0))
	case "enter", "space":
		if m.usb.cursor >= len(devs) {
			return m, nil
		}
		d := devs[m.usb.cursor]
		switch u := m.sharedUSB(d); {
		case d.Host == m.usb.host:
			m.flash, m.flashErr = fmt.Sprintf("%s is already shared with %s", d.Title(), d.Host), true
			return m, nil
		case d.Host != "" && !u.AdHoc:
			m.flash, m.flashErr = fmt.Sprintf("%s is shared with %s by profile %s; deactivate the profile to move it", d.Title(), d.Host, strings.Join(u.Profiles, ", ")), true
			return m, nil
		case d.Host != "":
			m.mode = modeNormal
			return m, m.call("moving "+d.Title()+" to "+m.usb.host, api.MethodUSBAttach, api.USBParams{Host: m.usb.host, Device: d.BusID, Move: true})
		}
		m.mode = modeNormal
		return m, m.share(d, m.usb.host)
	}
	return m, nil
}

func (m Model) usbView() []string {
	lines := []string{styleSection.Render("SHARE A USB DEVICE WITH " + m.usb.host), ""}
	if m.status.USBUnavailable != "" {
		lines = append(lines, styleWarn.Render("  ⚠ "+m.status.USBUnavailable), "")
	}
	for i, d := range m.status.USB {
		text := fmt.Sprintf("%-8s %-10s %s", d.BusID, d.ID, d.Name)
		u := m.sharedUSB(d)
		movable := d.Host != "" && d.Host != m.usb.host && u.AdHoc
		switch {
		case movable:
			text += "  (shared with " + d.Host + "; enter moves it here)"
		case d.Host != "" && !u.AdHoc:
			text += "  (shared with " + d.Host + " by profile " + strings.Join(u.Profiles, ", ") + ")"
		case d.Host != "":
			text += "  (shared with " + d.Host + ")"
		}
		switch {
		case i == m.usb.cursor:
			lines = append(lines, "▸ "+styleSelected.Render(text))
		case d.Host != "" && !movable:
			lines = append(lines, "  "+styleFaint.Render(text))
		default:
			lines = append(lines, "  "+text)
		}
	}
	return append(lines, "", styleFaint.Render("  While shared, a device is gone from this machine; it comes back when you stop sharing it."))
}

// kubePicker lists the services and pods kubectl on a host can forward to.
type kubePicker struct {
	host    string
	context string
	loading bool
	err     error
	res     *api.KubeTargetsResult
	filter  textinput.Model
	cursor  int
}

type kubeTargetsMsg struct {
	host, context string
	res           api.KubeTargetsResult
	err           error
}

// WithKubeForward opens the Kubernetes forward picker for host when the UI
// starts.
func (m Model) WithKubeForward(host string) Model {
	m.openKube(host)
	return m
}

// WithAdd opens the Add menu for host when the UI starts.
func (m Model) WithAdd(host string) Model {
	m.openAdd(host)
	return m
}

func openKubeFwd(m *Model, host string) tea.Cmd {
	m.openKube(host)
	return m.listKube()
}

func (m *Model) openKube(host string) {
	f := textinput.New()
	f.Prompt, f.Placeholder = "filter> ", "type to filter by namespace, kind or name"
	f.SetWidth(max(m.width-12, 10))
	f.Focus()
	m.mode, m.kube = modeKube, kubePicker{host: host, loading: true, filter: f}
}

func (m Model) listKube() tea.Cmd {
	c, host, kctx := m.client, m.kube.host, m.kube.context
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		defer cancel()
		var res api.KubeTargetsResult
		err := c.Call(ctx, api.MethodKubeTargets, api.KubeTargetsParams{Host: host, Context: kctx}, &res)
		return kubeTargetsMsg{host: host, context: kctx, res: res, err: err}
	}
}

func (m Model) gotKube(msg kubeTargetsMsg) Model {
	if msg.host != m.kube.host || msg.context != m.kube.context {
		return m
	}
	m.kube.loading, m.kube.err = false, msg.err
	if msg.err == nil {
		res := msg.res
		m.kube.res, m.kube.context = &res, res.Context
	}
	m.kube.cursor = 0
	return m
}

func (m Model) visibleKube() []api.KubeTarget {
	if m.kube.res == nil {
		return nil
	}
	terms := strings.Fields(strings.ToLower(m.kube.filter.Value()))
	var out []api.KubeTarget
	for _, t := range m.kube.res.Targets {
		hay := strings.ToLower(t.Ref() + " " + t.Owner)
		if !slices.ContainsFunc(terms, func(s string) bool { return !strings.Contains(hay, s) }) {
			out = append(out, t)
		}
	}
	return out
}

func (m Model) kubeKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	visible := m.visibleKube()
	switch msg.String() {
	case "esc":
		if m.kube.filter.Value() != "" {
			m.kube.filter.Reset()
			m.kube.cursor = 0
			return m, nil
		}
		m.mode = modeNormal
		return m, nil
	case "up", "ctrl+p":
		m.kube.cursor = max(m.kube.cursor-1, 0)
		return m, nil
	case "down", "ctrl+n":
		m.kube.cursor = min(m.kube.cursor+1, max(len(visible)-1, 0))
		return m, nil
	case "pgup":
		m.kube.cursor = max(m.kube.cursor-10, 0)
		return m, nil
	case "pgdown":
		m.kube.cursor = min(m.kube.cursor+10, max(len(visible)-1, 0))
		return m, nil
	case "ctrl+r":
		m.kube.loading, m.kube.err = true, nil
		return m, m.listKube()
	case "tab", "shift+tab":
		res := m.kube.res
		if res == nil || len(res.Contexts) < 2 {
			return m, nil
		}
		step := 1
		if msg.String() == "shift+tab" {
			step = len(res.Contexts) - 1
		}
		i := slices.Index(res.Contexts, res.Context)
		m.kube.context = res.Contexts[(i+step)%len(res.Contexts)]
		m.kube.loading, m.kube.err, m.kube.res = true, nil, nil
		return m, m.listKube()
	case "enter":
		if m.kube.cursor >= len(visible) {
			return m, nil
		}
		cmd := m.openForm(kubeForwardForm(m.kube.host, m.kube.res.Context, visible[m.kube.cursor]))
		return m, cmd
	}
	var cmd tea.Cmd
	before := m.kube.filter.Value()
	m.kube.filter, cmd = m.kube.filter.Update(msg)
	if m.kube.filter.Value() != before {
		m.kube.cursor = 0
	}
	return m, cmd
}

// kubeForwardForm asks which port of t to forward, and where.
func kubeForwardForm(host, kctx string, t api.KubeTarget) form {
	var portField formField
	if len(t.Ports) > 0 {
		var choices []string
		for _, p := range t.Ports {
			c := strconv.Itoa(p.Port)
			if p.Name != "" {
				c += " (" + p.Name + ")"
			}
			choices = append(choices, c)
		}
		portField = choiceField("Port", "←/→ to pick", choices...)
	} else {
		portField = textField("Port", "e.g. 8080", "", t.Kind+"/"+t.Name+" lists no ports; type one")
	}
	f := form{title: "FORWARD " + t.Kind + "/" + t.Name + " (" + t.Namespace + ") HERE", fromPicker: true, fields: []formField{
		portField,
		textField("Local port", "same", "", "empty: the same number; 0: any free one"),
		choiceField("Listen on", "←/→", listenChoices...),
		textField("Label", "", t.Name, labelHelp),
	}}
	f.build = func(v formValues) (formResult, error) {
		port, _, _ := strings.Cut(v[0], " ")
		tp, err := portValue(formValues{port}, 0, "", "the port")
		if err != nil {
			return formResult{}, err
		}
		lp, err := portValue(v, 1, tp, "the local port")
		if err != nil {
			return formResult{}, err
		}
		ref := forward.KubeRef{Context: kctx, Namespace: t.Namespace, Kind: t.Kind, Name: t.Name}
		return forwardResult(host, v.get(3, ""), "K:"+bindPrefix(v[2])+lp+":"+ref.String()+":"+tp)
	}
	return f
}

func (m Model) kubeView(height int) []string {
	title := "FORWARD FROM KUBERNETES via " + m.kube.host
	if res := m.kube.res; res != nil {
		title += " · context " + res.Context
		if len(res.Contexts) > 1 {
			title += fmt.Sprintf(" (%d of %d, tab for next)", slices.Index(res.Contexts, res.Context)+1, len(res.Contexts))
		}
	}
	lines := []string{styleSection.Render(title), m.kube.filter.View(), ""}
	switch {
	case m.kube.loading:
		return append(lines, "listing services and pods with kubectl on "+m.kube.host+"…")
	case m.kube.err != nil:
		return append(lines, styleErr.Render(m.kube.err.Error()), "", styleFaint.Render("ctrl+r to retry · d on the host runs doctor"))
	}
	visible := m.visibleKube()
	if len(visible) == 0 {
		return append(lines, "nothing matches")
	}
	nameW := len("NAMESPACE/KIND/NAME")
	for _, t := range visible {
		nameW = max(nameW, min(len(t.Ref()), 60))
	}
	lines = append(lines, styleFaint.Render(fmt.Sprintf("    %-*s  %-24s %s", nameW, "NAMESPACE/KIND/NAME", "PORTS", "OWNER")))
	rows := make([]string, len(visible))
	for i, t := range visible {
		var ports []string
		for _, p := range t.Ports {
			ports = append(ports, strconv.Itoa(p.Port))
		}
		text := fmt.Sprintf("%-*s  %-24s %s", nameW, t.Ref(), dash(strings.Join(ports, ",")), t.Owner)
		marker := "  "
		if i == m.kube.cursor {
			text, marker = styleSelected.Render(text), "▸ "
		}
		rows[i] = marker + "  " + text
	}
	avail := max(height-len(lines), 1)
	start := 0
	if m.kube.cursor >= avail {
		start = m.kube.cursor - avail + 1
	}
	return append(lines, rows[start:min(start+avail, len(rows))]...)
}
