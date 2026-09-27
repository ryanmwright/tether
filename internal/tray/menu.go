// Package tray is tether's system tray icon (a StatusNotifierItem, as used
// by KDE and most Linux desktops): the overall state at a glance, and a menu
// to bring hosts and profiles up and down.
package tray

import (
	"fmt"
	"strings"

	"github.com/ryanmwright/tether/internal/api"
)

// Item is one menu entry. The menu is rebuilt from the daemon's status on
// every change; IDs keep entries stable across rebuilds.
type Item struct {
	ID        string
	Title     string
	Enabled   bool
	Checkable bool
	Checked   bool
	Separator bool
	Action    *Action
	Children  []Item
}

// Action is what clicking an item does: a daemon call, or a local command.
type Action struct {
	Method string // api method; empty for Local
	Params any
	Done   string // said in a notification if the call fails ("connect devbox")
	Local  string // localOpenTUI, localConnectPrompt, localQuit, localStartDaemon
}

const (
	localOpenTUI       = "open-tui"
	localConnectPrompt = "connect-prompt"
	localQuit          = "quit"
	localStartDaemon   = "start-daemon"
)

func separator(id string) Item { return Item{ID: id, Separator: true} }

func label(id, title string) Item { return Item{ID: id, Title: title} }

func action(id, title string, a *Action) Item {
	return Item{ID: id, Title: title, Enabled: true, Action: a}
}

var stateMark = map[api.State]string{
	api.StateUp: "●", api.StatePending: "◌", api.StateDegraded: "◐", api.StateError: "✕", api.StateDown: "○",
}

// Summary is the overall state for the icon and a one-line tooltip. Only
// hosts that are wanted (not down) count.
func Summary(st api.Status) (api.State, string) {
	counts := map[api.State]int{}
	for _, h := range st.Hosts {
		counts[h.State]++
	}
	var parts []string
	for _, s := range []api.State{api.StateUp, api.StateDegraded, api.StatePending, api.StateError} {
		if n := counts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	overall := api.StateDown
	switch {
	case counts[api.StateError] > 0:
		overall = api.StateError
	case counts[api.StateDegraded] > 0, counts[api.StatePending] > 0:
		overall = api.StateDegraded
	case counts[api.StateUp] > 0:
		overall = api.StateUp
	}
	if len(parts) == 0 {
		return overall, "nothing connected"
	}
	return overall, strings.Join(parts, ", ")
}

// Build lays out the menu for a status.
func Build(st api.Status) []Item {
	_, summary := Summary(st)
	items := []Item{label("summary", "tether — "+summary)}
	if st.ConfigError != "" {
		first, _, _ := strings.Cut(st.ConfigError, "\n")
		items = append(items, label("config-error", "⚠ config error: "+first))
	}
	items = append(items, separator("sep-hosts"))
	if len(st.Hosts) == 0 {
		items = append(items, label("no-hosts", "No hosts configured"))
	}
	for _, h := range st.Hosts {
		items = append(items, hostItem(h))
	}
	items = append(items, action("connect", "Connect to host…", &Action{Local: localConnectPrompt}))

	if len(st.Profiles) > 0 {
		items = append(items, separator("sep-profiles"), label("profiles", "Profiles"))
		for _, p := range st.Profiles {
			title := p.Name
			if p.Active {
				title += " (" + string(p.State) + ")"
			}
			it := Item{ID: "profile:" + p.Name, Title: title, Enabled: true, Checkable: true, Checked: p.Active}
			if p.Active {
				it.Action = &Action{Method: api.MethodDown, Params: api.TargetParams{Name: p.Name, Kind: api.TargetProfile}, Done: "deactivate " + p.Name}
			} else {
				it.Action = &Action{Method: api.MethodUp, Params: api.TargetParams{Name: p.Name, Kind: api.TargetProfile}, Done: "activate " + p.Name}
			}
			items = append(items, it)
		}
	}

	return append(items,
		separator("sep-app"),
		action("open-tui", "Open terminal UI", &Action{Local: localOpenTUI}),
		action("reload", "Reload config", &Action{Method: api.MethodReload, Done: "reload the config"}),
		action("quit", "Quit tray", &Action{Local: localQuit}),
	)
}

func hostItem(h api.HostStatus) Item {
	id := "host:" + h.Name
	title := fmt.Sprintf("%s %s", stateMark[h.State], h.Name)
	if h.State != api.StateDown {
		title += " (" + string(h.State) + ")"
	}
	target := api.TargetParams{Name: h.Name, Kind: api.TargetHost}
	var children []Item
	if h.State == api.StateDown {
		children = append(children, action(id+":up", "Connect", &Action{Method: api.MethodUp, Params: target, Done: "connect " + h.Name}))
	} else {
		children = append(children, action(id+":down", "Disconnect", &Action{Method: api.MethodDown, Params: target, Done: "disconnect " + h.Name}))
	}
	if h.State == api.StateError {
		if h.Error != "" {
			children = append(children, label(id+":error", "✕ "+h.Error))
		}
		children = append(children, action(id+":retry", "Retry now", &Action{Method: api.MethodUp, Params: target, Done: "reconnect " + h.Name}))
	}

	if len(h.Forwards) > 0 || len(h.Mounts) > 0 {
		children = append(children, separator(id+":sep-items"))
	}
	gpgAdHoc := false
	for _, f := range h.Forwards {
		gpgAdHoc = gpgAdHoc || (f.Spec == "gpg-agent" && f.AdHoc)
		title := stateMark[f.State] + " " + f.Spec
		if f.Error != "" {
			title += " — " + f.Error
		} else if f.AllocatedPort != 0 {
			title += fmt.Sprintf(" — remote port %d", f.AllocatedPort)
		}
		children = append(children, label(id+":fwd:"+f.Spec, title))
	}
	for _, m := range h.Mounts {
		title := stateMark[m.State] + " " + m.Key
		if m.Error != "" {
			title += " — " + m.Error
		}
		children = append(children, label(id+":mount:"+m.Key, title))
	}

	children = append(children, separator(id+":sep-actions"))
	gpg := Item{ID: id + ":gpg", Title: "Forward gpg-agent", Enabled: true, Checkable: true, Checked: gpgAdHoc}
	if gpgAdHoc {
		gpg.Action = &Action{Method: api.MethodForwardRemove, Params: api.ForwardParams{Host: h.Name, Spec: "gpg-agent"}, Done: "stop gpg-agent forwarding to " + h.Name}
	} else {
		gpg.Action = &Action{Method: api.MethodForwardAdd, Params: api.ForwardParams{Host: h.Name, Spec: "gpg-agent"}, Done: "forward gpg-agent to " + h.Name}
	}
	children = append(children, gpg)
	if h.AdHoc {
		children = append(children, action(id+":forget", "Forget this host", &Action{Method: api.MethodHostRemove, Params: api.HostParams{Name: h.Name}, Done: "forget " + h.Name}))
	}
	return Item{ID: id, Title: title, Enabled: true, Children: children}
}

// DisconnectedMenu is shown while the daemon isn't reachable.
func DisconnectedMenu() []Item {
	return []Item{
		label("summary", "tether — daemon not running"),
		separator("sep"),
		action("start-daemon", "Start daemon", &Action{Local: localStartDaemon}),
		action("quit", "Quit tray", &Action{Local: localQuit}),
	}
}

// Shape is a menu's structure: IDs, nesting and item kinds. Menus with the
// same shape can be updated in place; others must be rebuilt.
func Shape(items []Item) string {
	var b strings.Builder
	var walk func([]Item, int)
	walk = func(items []Item, depth int) {
		for _, it := range items {
			fmt.Fprintf(&b, "%d|%s|%t|%t\n", depth, it.ID, it.Separator, it.Checkable)
			walk(it.Children, depth+1)
		}
	}
	walk(items, 0)
	return b.String()
}
