package tray

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"fyne.io/systray"

	"github.com/ryanmwright/tether/internal/api"
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
		if it.Action != nil {
			go t.watch(it.ID, mi, t.buildDone)
		}
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
		t.report("open the terminal UI", t.openTUI())
		return
	case localConnectPrompt:
		t.report("connect", t.connectPrompt())
		return
	}
	t.report(a.Done, t.call(a.Method, a.Params))
}

func (t *tray) call(method string, params any) error {
	t.mu.Lock()
	c := t.client
	t.mu.Unlock()
	if c == nil {
		return errors.New("the daemon isn't running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return c.Call(ctx, method, params, nil)
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

func (t *tray) openTUI() error {
	term := terminalCommand(t.opts.Terminal)
	if term == nil {
		return errors.New("no terminal emulator found; set one with `tether tray --terminal`")
	}
	return spawn(append(term, t.opts.TetherPath, "tui")...)
}

// connectPrompt asks for "NAME [SSH-DEST]" with the desktop's dialog tool,
// or opens the terminal UI (where c does the same) if there's none.
func (t *tray) connectPrompt() error {
	var dialog []string
	switch {
	case lookPath("kdialog"):
		dialog = []string{"kdialog", "--title", "tether", "--inputbox", "Connect to host (NAME, or NAME SSH-DEST):"}
	case lookPath("zenity"):
		dialog = []string{"zenity", "--entry", "--title=tether", "--text=Connect to host (NAME, or NAME SSH-DEST):"}
	default:
		return t.openTUI()
	}
	out, err := exec.Command(dialog[0], dialog[1:]...).Output()
	if err != nil {
		return nil // cancelled
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 || len(fields) > 2 {
		return nil
	}
	p := api.HostParams{Name: fields[0]}
	if len(fields) == 2 {
		p.SSH = fields[1]
	}
	return t.call(api.MethodHostAdd, p)
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
