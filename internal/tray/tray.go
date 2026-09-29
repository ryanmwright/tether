package tray

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/mount"
	"github.com/ryanmwright/tether/internal/rpc"
)

const (
	reconnectInterval = 3 * time.Second
	callTimeout       = 60 * time.Second
)

type Options struct {
	// Connect reaches the daemon, starting it first if autostart is set.
	Connect func(ctx context.Context, autostart bool) (*rpc.Client, error)
	// TetherPath is the tether binary, used to open the terminal UI.
	TetherPath string
	// Terminal is the command that runs a program in a terminal, e.g.
	// ["konsole", "-e"]. Empty means detect one.
	Terminal []string
	Notify   bool
	Log      *slog.Logger
}

type tray struct {
	opts     Options
	notifier *Notifier

	mu      sync.Mutex
	client  *rpc.Client
	actions map[string]*Action

	// Only touched by the goroutine running loop.
	shape     string
	items     map[string]*systray.MenuItem
	buildDone chan struct{}

	startDaemon chan struct{}
}

// Run shows the tray icon until ctx is cancelled or the user quits it.
func Run(ctx context.Context, opts Options) error {
	t := &tray{opts: opts, actions: map[string]*Action{}, startDaemon: make(chan struct{}, 1)}
	if opts.Notify {
		n, err := NewNotifier()
		if err != nil {
			opts.Log.Warn("desktop notifications unavailable", "err", err)
		} else {
			t.notifier = n
			defer n.Close()
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		systray.Quit()
	}()
	systray.Run(func() {
		systray.SetTitle("tether")
		systray.SetOnTapped(func() { t.run(&Action{Local: localOpenTUI}) })
		t.render(DisconnectedMenu(), LookIdle, "tether: connecting to the daemon…")
		go t.loop(ctx)
	}, nil)
	return nil
}

// loop keeps a connection to the daemon and renders every status it sends.
func (t *tray) loop(ctx context.Context) {
	autostart := true
	for ctx.Err() == nil {
		c, err := t.opts.Connect(ctx, autostart)
		autostart = false
		if err != nil {
			t.opts.Log.Debug("daemon not reachable", "err", err)
			t.render(DisconnectedMenu(), LookIdle, "tether: daemon not running")
			select {
			case <-ctx.Done():
				return
			case <-t.startDaemon:
				autostart = true
			case <-time.After(reconnectInterval):
			}
			continue
		}
		if err := c.Call(ctx, api.MethodSubscribe, api.SubscribeParams{}, nil); err != nil {
			c.Close()
			continue
		}
		t.setClient(c)
		t.follow(c)
		t.setClient(nil)
		c.Close()
	}
}

// follow renders statuses until the connection ends.
func (t *tray) follow(c *rpc.Client) {
	var prev *api.Status
	for n := range c.Notifications() {
		if n.Method != api.EventStatus {
			continue
		}
		var st api.Status
		if err := json.Unmarshal(n.Params, &st); err != nil {
			continue
		}
		look, summary := Summary(st)
		t.render(Build(st), look, "tether: "+summary)
		if prev != nil && t.notifier != nil {
			for _, notice := range Changes(*prev, st) {
				if err := t.notifier.Show(notice); err != nil {
					t.opts.Log.Warn("notification failed", "err", err)
				}
			}
		}
		prev = &st
	}
}

func (t *tray) setClient(c *rpc.Client) {
	t.mu.Lock()
	t.client = c
	t.mu.Unlock()
}

// render shows items, updating the existing menu in place when its shape is
// unchanged (so an open menu doesn't jump) and rebuilding it otherwise.
func (t *tray) render(items []Item, look Look, tooltip string) {
	systray.SetIcon(Icon(look))
	systray.SetTooltip(tooltip)

	t.mu.Lock()
	clear(t.actions)
	collectActions(items, t.actions)
	t.mu.Unlock()

	if shape := Shape(items); shape != t.shape {
		if t.buildDone != nil {
			close(t.buildDone)
		}
		systray.ResetMenu()
		t.shape, t.items, t.buildDone = shape, map[string]*systray.MenuItem{}, make(chan struct{})
		t.build(nil, items)
		return
	}
	t.update(items)
}

func collectActions(items []Item, into map[string]*Action) {
	for _, it := range items {
		if it.Action != nil {
			into[it.ID] = it.Action
		}
		collectActions(it.Children, into)
	}
}

func (t *tray) build(parent *systray.MenuItem, items []Item) {
	for _, it := range items {
		if it.Separator {
			if parent == nil {
				systray.AddSeparator()
			} else {
				parent.AddSeparator()
			}
			continue
		}
		var mi *systray.MenuItem
		switch {
		case parent == nil && it.Checkable:
			mi = systray.AddMenuItemCheckbox(it.Title, "", it.Checked)
		case parent == nil:
			mi = systray.AddMenuItem(it.Title, "")
		case it.Checkable:
			mi = parent.AddSubMenuItemCheckbox(it.Title, "", it.Checked)
		default:
			mi = parent.AddSubMenuItem(it.Title, "")
		}
		if !it.Enabled {
			mi.Disable()
		}
		t.items[it.ID] = mi
		// Watch every item: one without an action now may get one in an
		// in-place update.
		go t.watch(it.ID, mi, t.buildDone)
		t.build(mi, it.Children)
	}
}

func (t *tray) update(items []Item) {
	for _, it := range items {
		mi := t.items[it.ID]
		if mi == nil {
			continue // separators
		}
		mi.SetTitle(it.Title)
		if it.Enabled {
			mi.Enable()
		} else {
			mi.Disable()
		}
		if it.Checkable {
			if it.Checked {
				mi.Check()
			} else {
				mi.Uncheck()
			}
		}
		t.update(it.Children)
	}
}

// watch runs an item's current action on each click, until the menu is
// rebuilt.
func (t *tray) watch(id string, mi *systray.MenuItem, done chan struct{}) {
	for {
		select {
		case <-mi.ClickedCh:
			t.mu.Lock()
			a := t.actions[id]
			t.mu.Unlock()
			if a != nil {
				go t.run(a)
			}
		case <-done:
			return
		}
	}
}

func (t *tray) run(a *Action) {
	switch a.Local {
	case localQuit:
		systray.Quit()
		return
	case localStartDaemon:
		poke(t.startDaemon)
		return
	case localOpenTUI:
		var args []string
		if a.Arg != "" {
			args = []string{a.Arg, a.Host}
		}
		t.report("open the terminal UI", t.openTUI(args...))
		return
	case localAdd:
		t.report("add to "+a.Host, t.addPrompt(a.Host, a.Arg))
		return
	case localCopy:
		t.report("copy "+a.Arg, copyText(a.Arg))
		return
	case localOpenURL:
		t.report("open "+a.Arg, spawn("xdg-open", a.Arg))
		return
	case localConnectPrompt:
		t.report("connect", t.connectPrompt())
		return
	}
	t.report(a.Done, t.call(a.Method, a.Params))
}

func (t *tray) call(method string, params any) error {
	return t.callResult(method, params, nil)
}

func (t *tray) callResult(method string, params, result any) error {
	t.mu.Lock()
	c := t.client
	t.mu.Unlock()
	if c == nil {
		return errors.New("the daemon isn't running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return c.Call(ctx, method, params, result)
}

// addPrompt asks, with the desktop's dialogs, for what's needed to add kind
// to host, and adds it. Without a dialog tool it opens the terminal UI at
// the same place.
func (t *tray) addPrompt(host, kind string) error {
	if dialogTool() == "" {
		switch kind {
		case addPVC:
			return t.openTUI("--pvc", host)
		case addKube:
			return t.openTUI("--k8s", host)
		}
		return t.openTUI("--add", host)
	}
	add := func(spec string) error {
		return t.call(api.MethodForwardAdd, api.ForwardParams{Host: host, Spec: spec})
	}
	ask := inputDialog
	switch kind {
	case addLocal:
		rp, ok := ask("Port on "+host+" to reach from here:", "")
		if !ok {
			return nil
		}
		lp, ok := ask("Local port (connect to localhost:PORT here):", rp)
		if !ok {
			return nil
		}
		return add("L:" + lp + ":localhost:" + rp)
	case addRemote:
		lp, ok := ask("Port here for "+host+" to reach:", "")
		if !ok {
			return nil
		}
		rp, ok := ask("Port on "+host+" (programs there connect to localhost:PORT):", lp)
		if !ok {
			return nil
		}
		return add("R:" + rp + ":localhost:" + lp)
	case addToHost:
		target, ok := ask("Machine and port, as "+host+" sees them (e.g. db.internal:5432):", "")
		if !ok {
			return nil
		}
		_, tp, _ := cutLast(target, ":")
		lp, ok := ask("Local port (connect to localhost:PORT here):", tp)
		if !ok {
			return nil
		}
		return add(lp + ":" + target)
	case addFromLAN:
		target, ok := ask("Machine and port on your network (e.g. nas.lan:9000):", "")
		if !ok {
			return nil
		}
		_, tp, _ := cutLast(target, ":")
		rp, ok := ask("Port on "+host+" (programs there connect to localhost:PORT):", tp)
		if !ok {
			return nil
		}
		return add("R:" + rp + ":" + target)
	case addSOCKS, addHTTP:
		letter, def, what := "D", "1080", "SOCKS"
		if kind == addHTTP {
			letter, def, what = "H", "8080", "HTTP"
		}
		lp, ok := ask(what+" proxy on local port (0: any free one):", def)
		if !ok {
			return nil
		}
		return add(letter + ":" + lp)
	case addReverseSOCKS:
		rp, ok := ask("SOCKS proxy on "+host+", port (programs there use localhost:PORT):", "1080")
		if !ok {
			return nil
		}
		return add("R:" + rp)
	case addSpec:
		spec, ok := ask("Forward to add to "+host+" (e.g. 5432, db.internal:5432, R:3000:localhost:3000, socks, rsocks, http, K:8080:ns/svc/name:80; name it with LABEL=):", "")
		if !ok {
			return nil
		}
		return add(spec)
	case addMountHere, addMountThere:
		return t.mountPrompt(host, kind == addMountThere)
	case addKube:
		return t.kubeForwardPrompt(host)
	case addPVC:
		return t.pvcPrompt(host)
	}
	return fmt.Errorf("unknown kind %q", kind)
}

// remoteDirDialog browses host's directories with list dialogs, one level
// at a time, starting in its home; the user can also type a path.
func (t *tray) remoteDirDialog(host string) (string, bool) {
	dir := "~"
	for {
		var res api.FSListResult
		if err := t.callResult(api.MethodFSList, api.FSListParams{Host: host, Path: dir}, &res); err != nil {
			t.report("list "+dir+" on "+host, err)
			return inputDialog("Directory on "+host+" to mount here (relative to the remote home unless absolute):", "~/")
		}
		items := []string{"✓ Mount " + res.Path, "✎ Type a path…"}
		if res.Path != "/" {
			items = append(items, "↑ ..")
		}
		var dirs []string
		for _, d := range res.Dirs {
			if !strings.HasPrefix(d, ".") {
				dirs = append(dirs, d)
				items = append(items, d+"/")
			}
		}
		i, ok := listDialog("Directory on "+host+" to mount here:", items)
		if !ok {
			return "", false
		}
		switch {
		case i == 0:
			return res.Path, true
		case i == 1:
			return inputDialog("Directory on "+host+" to mount here (relative to the remote home unless absolute):", res.Path)
		case items[i] == "↑ ..":
			dir = path.Dir(res.Path)
		default:
			dir = path.Join(res.Path, dirs[i-len(items)+len(dirs)])
		}
	}
}

// kubeForwardPrompt picks a service or pod kubectl on host can reach, and a
// port, and forwards it here.
func (t *tray) kubeForwardPrompt(host string) error {
	var res api.KubeTargetsResult
	if err := t.callResult(api.MethodKubeTargets, api.KubeTargetsParams{Host: host}, &res); err != nil {
		return err
	}
	if len(res.Targets) == 0 {
		return fmt.Errorf("no services or pods in context %s", res.Context)
	}
	var names []string
	for _, tg := range res.Targets {
		var ports []string
		for _, p := range tg.Ports {
			ports = append(ports, strconv.Itoa(p.Port))
		}
		names = append(names, tg.Ref()+"   "+strings.Join(ports, ","))
	}
	i, ok := listDialog("Forward from Kubernetes (context "+res.Context+", via "+host+"):", names)
	if !ok {
		return nil
	}
	tg := res.Targets[i]
	port := ""
	switch len(tg.Ports) {
	case 0:
		if port, ok = inputDialog("Port of "+tg.Kind+"/"+tg.Name+" to forward:", ""); !ok {
			return nil
		}
	case 1:
		port = strconv.Itoa(tg.Ports[0].Port)
	default:
		var ports []string
		for _, p := range tg.Ports {
			ports = append(ports, strings.TrimSpace(fmt.Sprintf("%d %s", p.Port, p.Name)))
		}
		j, ok := listDialog("Which port of "+tg.Kind+"/"+tg.Name+"?", ports)
		if !ok {
			return nil
		}
		port = strconv.Itoa(tg.Ports[j].Port)
	}
	lp, ok := inputDialog("Local port (connect to localhost:PORT here):", port)
	if !ok {
		return nil
	}
	spec := fmt.Sprintf("%s=K:%s:%s/%s:%s", tg.Name, lp, res.Context, tg.Ref(), port)
	return t.call(api.MethodForwardAdd, api.ForwardParams{Host: host, Spec: spec})
}

// pvcPrompt picks a claim kubectl on host can see, and a mount point, and
// mounts it here.
func (t *tray) pvcPrompt(host string) error {
	var res api.KubeListResult
	if err := t.callResult(api.MethodKubeList, api.KubeListParams{Host: host}, &res); err != nil {
		return err
	}
	var pvcs []api.PVC
	var names []string
	for _, p := range res.PVCs {
		if p.Mountable {
			pvcs = append(pvcs, p)
			names = append(names, strings.TrimSpace(fmt.Sprintf("%s   %s %s   %s", p.Ref(), p.Phase, p.Size(), p.Note)))
		}
	}
	if len(pvcs) == 0 {
		return fmt.Errorf("no claims that can be mounted in context %s", res.Context)
	}
	i, ok := listDialog("Mount a Kubernetes claim (context "+res.Context+", via "+host+"):", names)
	if !ok {
		return nil
	}
	p := pvcs[i]
	def := path.Join(res.MountRoot, strings.ReplaceAll(res.Context, "/", "_"), p.Namespace, p.Name)
	local, ok := inputDialog("Mount "+p.Ref()+" here at:", def)
	if !ok {
		return nil
	}
	return t.call(api.MethodMountAdd, api.MountParams{Host: host, Direction: "pvc-to-local", Local: local,
		Kube: &api.KubeMount{Context: res.Context, Namespace: p.Namespace, PVC: p.Name}})
}

func cutLast(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

// report tells the user about a failed action; successes show in the menu.
func (t *tray) report(what string, err error) {
	if err == nil {
		return
	}
	t.opts.Log.Warn("action failed", "action", what, "err", err)
	if t.notifier != nil {
		t.notifier.Show(Notice{Key: "action", Summary: "Couldn't " + what, Body: err.Error()})
	}
}

// openTUI opens the terminal UI in a terminal, with extra arguments to
// `tether tui`.
func (t *tray) openTUI(args ...string) error {
	term := terminalCommand(t.opts.Terminal)
	if term == nil {
		return errors.New("no terminal emulator found; set one with `tether tray --terminal`")
	}
	return spawn(append(append(term, t.opts.TetherPath, "tui"), args...)...)
}

// connectPrompt asks for "NAME [SSH-DEST]" with the desktop's dialog tool,
// or opens the terminal UI (where c does the same) if there's none.
func (t *tray) connectPrompt() error {
	if dialogTool() == "" {
		return t.openTUI()
	}
	answer, ok := inputDialog("Connect to host (NAME, or NAME SSH-DEST):", "")
	fields := strings.Fields(answer)
	if !ok || len(fields) == 0 || len(fields) > 2 {
		return nil
	}
	p := api.HostParams{Name: fields[0]}
	if len(fields) == 2 {
		p.SSH = fields[1]
	}
	return t.call(api.MethodHostAdd, p)
}

// mountPrompt asks for the two sides of a mount on host: the local directory
// with a folder picker when it must exist (mounting it on the remote), and
// the rest in input boxes. Without a dialog tool it opens the terminal UI
// (where m does the same).
func (t *tray) mountPrompt(host string, localToRemote bool) error {
	if dialogTool() == "" {
		return t.openTUI()
	}
	home, _ := os.UserHomeDir()
	p := api.MountParams{Host: host, Direction: string(mount.RemoteToLocal)}
	var ok bool
	if localToRemote {
		p.Direction = string(mount.LocalToRemote)
		if p.Local, ok = directoryDialog("Local directory to mount on "+host, home); !ok {
			return nil
		}
		if p.Remote, ok = inputDialog("Mount "+p.Local+" on "+host+" at:", "~/"+filepath.Base(p.Local)); !ok {
			return nil
		}
	} else {
		if p.Remote, ok = t.remoteDirDialog(host); !ok {
			return nil
		}
		base := path.Base(strings.TrimRight(p.Remote, "/"))
		if base == "~" || base == "." || base == "/" {
			base = host
		}
		if p.Local, ok = inputDialog("Mount "+host+":"+p.Remote+" here at:", "~/mnt/"+base); !ok {
			return nil
		}
	}
	// The daemon expands ~/; other relative paths are from our home, as the
	// tray has no meaningful working directory.
	if p.Local != "~" && !strings.HasPrefix(p.Local, "~/") && !filepath.IsAbs(p.Local) {
		p.Local = filepath.Join(home, p.Local)
	}
	return t.call(api.MethodMountAdd, p)
}

// dialogTool is the desktop's dialog program: kdialog or zenity, or "".
func dialogTool() string {
	for _, name := range []string{"kdialog", "zenity"} {
		if lookPath(name) {
			return name
		}
	}
	return ""
}

// inputDialog asks for a line of text; ok is false if cancelled or empty.
func inputDialog(text, initial string) (answer string, ok bool) {
	var argv []string
	switch dialogTool() {
	case "kdialog":
		argv = []string{"kdialog", "--title", "tether", "--inputbox", text, initial}
	case "zenity":
		argv = []string{"zenity", "--entry", "--title=tether", "--text=" + text, "--entry-text=" + initial}
	default:
		return "", false
	}
	return runDialog(argv)
}

// listDialog picks one of items, returning its index.
func listDialog(text string, items []string) (int, bool) {
	var argv []string
	switch dialogTool() {
	case "kdialog":
		argv = []string{"kdialog", "--title", "tether", "--menu", text}
		for i, it := range items {
			argv = append(argv, strconv.Itoa(i), it)
		}
	case "zenity":
		argv = []string{"zenity", "--list", "--title=tether", "--text=" + text, "--hide-header",
			"--column=i", "--column=item", "--hide-column=1", "--print-column=1", "--height=480", "--width=640"}
		for i, it := range items {
			argv = append(argv, strconv.Itoa(i), it)
		}
	default:
		return 0, false
	}
	answer, ok := runDialog(argv)
	if !ok {
		return 0, false
	}
	i, err := strconv.Atoi(answer)
	if err != nil || i < 0 || i >= len(items) {
		return 0, false
	}
	return i, true
}

// copyText puts s on the clipboard with whichever of wl-copy, xclip and
// xsel is installed.
func copyText(s string) error {
	for _, argv := range [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "--clipboard", "--input"}} {
		if !lookPath(argv[0]) {
			continue
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin = strings.NewReader(s)
		return cmd.Run()
	}
	return errors.New("no clipboard tool found; install wl-clipboard (Wayland) or xclip (X11)")
}

// directoryDialog picks an existing directory, starting in start.
func directoryDialog(title, start string) (dir string, ok bool) {
	var argv []string
	switch dialogTool() {
	case "kdialog":
		argv = []string{"kdialog", "--title", title, "--getexistingdirectory", start}
	case "zenity":
		argv = []string{"zenity", "--file-selection", "--directory", "--title=" + title, "--filename=" + start + "/"}
	default:
		return "", false
	}
	return runDialog(argv)
}

// runDialog runs a dialog and returns what it printed; a non-zero exit is a
// cancel.
func runDialog(argv []string) (string, bool) {
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	answer := strings.TrimSpace(string(out))
	return answer, err == nil && answer != ""
}

// terminalCommand is the prefix that runs a command in a terminal window.
func terminalCommand(preferred []string) []string {
	if len(preferred) > 0 {
		return preferred
	}
	if term := os.Getenv("TERMINAL"); term != "" && lookPath(term) {
		return []string{term, "-e"}
	}
	for _, c := range [][]string{
		{"konsole", "-e"}, {"kgx", "--"}, {"gnome-terminal", "--"}, {"kitty"}, {"alacritty", "-e"},
		{"foot"}, {"wezterm", "start", "--"}, {"xfce4-terminal", "-x"}, {"xterm", "-e"},
	} {
		if lookPath(c[0]) {
			return c
		}
	}
	return nil
}

func lookPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// spawn starts a detached program and reaps it when it exits.
func spawn(argv ...string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
