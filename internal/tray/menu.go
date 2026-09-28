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
	Local  string // localOpenTUI, localConnectPrompt, localMountHere, localMountThere, localQuit, localStartDaemon
	Host   string // the host a local action is for
}

const (
	localOpenTUI       = "open-tui"
	localConnectPrompt = "connect-prompt"
	localMountHere     = "mount-here"  // a remote directory, mounted here
	localMountThere    = "mount-there" // a local directory, mounted on the remote
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

// Look is the icon's overall state.
type Look string

const (
	LookUp      Look = "up"      // every host connected and healthy
	LookPartial Look = "partial" // some hosts connected, others disconnected
	LookBusy    Look = "busy"    // connecting, or connected with problems
	LookError   Look = "error"   // a connection is failing
	LookIdle    Look = "idle"    // nothing connected
)

// Summary picks the icon's look and a one-line description with counts:
// "4 hosts: 2 up, 1 failed, 1 disconnected · 1/2 profiles active".
func Summary(st api.Status) (Look, string) {
	counts := map[api.State]int{}
	for _, h := range st.Hosts {
		counts[h.State]++
	}
	connected := counts[api.StateUp] + counts[api.StateDegraded]

	look := LookIdle
	switch {
	case counts[api.StateError] > 0:
		look = LookError
	case counts[api.StatePending] > 0, counts[api.StateDegraded] > 0:
		look = LookBusy
	case connected > 0 && connected == len(st.Hosts):
		look = LookUp
	case connected > 0:
		look = LookPartial
	}

	text := "no hosts"
	if len(st.Hosts) > 0 {
		var parts []string
		for _, c := range []struct {
			state api.State
			word  string
		}{
			{api.StateUp, "up"}, {api.StateDegraded, "degraded"}, {api.StatePending, "connecting"},
			{api.StateError, "failed"}, {api.StateDown, "disconnected"},
		} {
			if n := counts[c.state]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, c.word))
			}
		}
		text = fmt.Sprintf("%d %s: %s", len(st.Hosts), plural(len(st.Hosts), "host", "hosts"), strings.Join(parts, ", "))
	}
	if len(st.Profiles) > 0 {
		active := 0
		for _, p := range st.Profiles {
			if p.Active {
				active++
			}
		}
		text += fmt.Sprintf(" · %d/%d %s active", active, len(st.Profiles), plural(len(st.Profiles), "profile", "profiles"))
	}
	return look, text
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
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

	items = append(items, usbItems(st)...)

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

	if len(h.Forwards) > 0 || len(h.Mounts) > 0 || len(h.USB) > 0 {
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
		it := label(id+":mount:"+m.Key, title)
		if m.AdHoc {
			it.Enabled = true
			it.Children = []Item{action(it.ID+":rm", "Unmount", &Action{
				Method: api.MethodMountRemove,
				Params: api.MountParams{Host: h.Name, Direction: m.Direction, Remote: m.Remote, Local: m.Local},
				Done:   "unmount " + m.Key,
			})}
		}
		children = append(children, it)
	}
	for _, u := range h.USB {
		title := stateMark[u.State] + " USB " + u.Device
		switch {
		case u.Error != "":
			title += " — " + u.Error
		case u.Name != "" && u.Device != u.BusID:
			title += " — " + u.Name + " at " + u.BusID
		case u.Name != "":
			title += " — " + u.Name
		}
		children = append(children, label(id+":usb:"+u.Device, title))
	}

	children = append(children, separator(id+":sep-actions"))
	gpg := Item{ID: id + ":gpg", Title: "Forward gpg-agent", Enabled: true, Checkable: true, Checked: gpgAdHoc}
	if gpgAdHoc {
		gpg.Action = &Action{Method: api.MethodForwardRemove, Params: api.ForwardParams{Host: h.Name, Spec: "gpg-agent"}, Done: "stop gpg-agent forwarding to " + h.Name}
	} else {
		gpg.Action = &Action{Method: api.MethodForwardAdd, Params: api.ForwardParams{Host: h.Name, Spec: "gpg-agent"}, Done: "forward gpg-agent to " + h.Name}
	}
	children = append(children, gpg,
		action(id+":mount-here", "Mount remote directory here…", &Action{Local: localMountHere, Host: h.Name}),
		action(id+":mount-there", "Mount local directory on "+h.Name+"…", &Action{Local: localMountThere, Host: h.Name}),
	)
	if h.AdHoc {
		children = append(children, action(id+":forget", "Forget this host", &Action{Method: api.MethodHostRemove, Params: api.HostParams{Name: h.Name}, Done: "forget " + h.Name}))
	}
	return Item{ID: id, Title: title, Enabled: true, Children: children}
}

// usbItems is the USB section: each local device, with a checkbox per host
// to share it there.
func usbItems(st api.Status) []Item {
	items := []Item{separator("sep-usb"), label("usb", "USB devices")}
	if st.USBUnavailable != "" {
		items = append(items, label("usb-unavailable", "⚠ "+st.USBUnavailable))
	}
	if len(st.USB) == 0 {
		return append(items, label("no-usb", "No USB devices"))
	}
	for _, d := range st.USB {
		id := "usb:" + d.BusID
		title := "○ " + d.Title()
		var children []Item
		for _, h := range st.Hosts {
			u, shared := sharedWith(h, d)
			it := Item{ID: id + ":" + h.Name, Title: "Share with " + h.Name, Enabled: true, Checkable: true, Checked: shared}
			switch {
			case shared && !u.AdHoc:
				it.Title += " (profile " + strings.Join(u.Profiles, ", ") + ")"
				it.Enabled = false
			case shared:
				it.Action = &Action{Method: api.MethodUSBDetach, Params: api.USBParams{Host: h.Name, Device: u.Device}, Done: "stop sharing " + d.Title() + " with " + h.Name}
			case d.Host != "":
				it.Enabled = false // it's elsewhere
			default:
				it.Action = &Action{Method: api.MethodUSBAttach, Params: api.USBParams{Host: h.Name, Device: d.BusID}, Done: "share " + d.Title() + " with " + h.Name}
			}
			if shared {
				title = stateMark[u.State] + " " + d.Title() + " — " + h.Name
				if u.State != api.StateUp {
					title += " (" + string(u.State) + ")"
				}
			}
			children = append(children, it)
		}
		if len(children) == 0 {
			children = append(children, label(id+":no-hosts", "No hosts to share with"))
		}
		items = append(items, Item{ID: id, Title: title, Enabled: true, Children: children})
	}
	return items
}

// sharedWith finds the entry on h, if any, that shares local device d:
// attached to it, or asked for by its bus ID or vendor:product.
func sharedWith(h api.HostStatus, d api.USBDevice) (api.USBStatus, bool) {
	for _, u := range h.USB {
		if u.BusID == d.BusID || u.Device == d.BusID {
			return u, true
		}
	}
	for _, u := range h.USB {
		if u.Device == d.ID && u.BusID == "" {
			return u, true
		}
	}
	return api.USBStatus{}, false
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
