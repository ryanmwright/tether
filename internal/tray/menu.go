// Package tray is tether's system tray icon (a StatusNotifierItem, as used
// by KDE and most Linux desktops): the overall state at a glance, and a menu
// to bring hosts and profiles up and down.
package tray

import (
	"fmt"
	"slices"
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
	Local  string // localOpenTUI, localConnectPrompt, localAdd, localCopy, localOpenURL, localQuit, localStartDaemon
	Host   string // the host a local action is for
	Arg    string // what to add (localAdd), the text to copy, the URL to open; TUI flag for localOpenTUI
}

const (
	localOpenTUI       = "open-tui"
	localConnectPrompt = "connect-prompt"
	localAdd           = "add"      // ask what to add to a host (Arg: which kind)
	localCopy          = "copy"     // copy Arg to the clipboard
	localOpenURL       = "open-url" // open Arg in the browser
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
	// The local host isn't a connection: count it only while it's in use.
	var hosts []api.HostStatus
	for _, h := range st.Hosts {
		if !h.Local || h.State != api.StateDown {
			hosts = append(hosts, h)
		}
	}
	st.Hosts = hosts
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
	for _, h := range st.Hosts {
		if by := gpgUsedBy(h); by != "" {
			text += " · gpg on " + h.Title() + " in use by " + by
		}
	}
	return look, text
}

// gpgOn reports whether this machine's gpg-agent is forwarded to h.
func gpgOn(h api.HostStatus) bool {
	for _, f := range h.Forwards {
		if isGPG(f) && f.State == api.StateUp {
			return true
		}
	}
	return false
}

// gpgUsedBy is what gpg on h uses instead of this machine's forwarded
// agent: another machine's, or its own; empty if it uses this one's, or
// this one's isn't forwarded.
func gpgUsedBy(h api.HostStatus) string {
	for _, f := range h.Forwards {
		if isGPG(f) && f.State == api.StateUp && f.UsedBy != "" {
			return f.UsedBy
		}
	}
	return ""
}

func isGPG(f api.ForwardStatus) bool { return f.Spec == "gpg-agent" || f.Spec == "gpg-ssh" }

func gpgClaimAction(h api.HostStatus) *Action {
	return &Action{Method: api.MethodGPGClaim, Params: api.GPGClaimParams{Host: h.Name}, Done: "use this machine's gpg keys on " + h.Title()}
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
	// A host using another machine's keys is worth a click from the top.
	for _, h := range st.Hosts {
		if by := gpgUsedBy(h); by != "" {
			items = append(items, action("gpg-claim:"+h.Name, "⇄ Use this machine's gpg keys on "+h.Title()+" (in use by "+by+")", gpgClaimAction(h)))
		}
	}
	items = append(items, separator("sep-hosts"))
	if len(st.Hosts) == 0 {
		items = append(items, label("no-hosts", "No hosts configured"))
	}
	for _, h := range st.Hosts {
		items = append(items, hostItem(h, st.USB))
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

func hostItem(h api.HostStatus, usb []api.USBDevice) Item {
	id := "host:" + h.Name
	title := fmt.Sprintf("%s %s", stateMark[h.State], h.Title())
	if h.State != api.StateDown {
		title += " (" + string(h.State) + ")"
	}
	target := api.TargetParams{Name: h.Name, Kind: api.TargetHost}
	var children []Item
	if h.State == api.StateDown {
		children = append(children, action(id+":up", "Connect", &Action{Method: api.MethodUp, Params: target, Done: "connect " + h.Title()}))
	} else {
		children = append(children, action(id+":down", "Disconnect", &Action{Method: api.MethodDown, Params: target, Done: "disconnect " + h.Title()}))
	}
	if h.State == api.StateError {
		if h.Error != "" {
			children = append(children, label(id+":error", "✕ "+h.Error))
		}
		children = append(children, action(id+":retry", "Retry now", &Action{Method: api.MethodUp, Params: target, Done: "reconnect " + h.Title()}))
	}

	if len(h.Forwards) > 0 || len(h.Mounts) > 0 || len(h.USB) > 0 {
		children = append(children, separator(id+":sep-items"))
	}
	gpgAdHoc := false
	for _, f := range h.Forwards {
		gpgAdHoc = gpgAdHoc || (f.Spec == "gpg-agent" && f.AdHoc)
		children = append(children, forwardItem(id, h.Name, f))
	}
	for _, m := range h.Mounts {
		title := stateMark[m.State] + " " + m.Key
		if m.Error != "" {
			title += " — " + m.Error
		}
		it := label(id+":mount:"+m.Key, title)
		if m.AdHoc {
			p := api.MountParams{Host: h.Name, Direction: m.Direction, Remote: m.Remote, Local: m.Local}
			if m.Kube != nil {
				k := m.Kube.KubeMount
				p.Remote, p.Kube = "", &k
			}
			it.Enabled = true
			it.Children = []Item{action(it.ID+":rm", "Unmount", &Action{Method: api.MethodMountRemove, Params: p, Done: "unmount " + m.Key})}
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

	children = append(children, separator(id+":sep-actions"), addItem(id, h, usb))
	if recent := recentItems(id, h); len(recent) > 0 {
		children = append(children, Item{ID: id + ":recent", Title: "Recent", Enabled: true, Children: recent})
	}
	if h.Local {
		return Item{ID: id, Title: title, Enabled: true, Children: children}
	}
	gpg := Item{ID: id + ":gpg", Title: "Forward gpg-agent", Enabled: true, Checkable: true, Checked: gpgAdHoc}
	if gpgAdHoc {
		gpg.Action = &Action{Method: api.MethodForwardRemove, Params: api.ForwardParams{Host: h.Name, Spec: "gpg-agent"}, Done: "stop gpg-agent forwarding to " + h.Title()}
	} else {
		gpg.Action = &Action{Method: api.MethodForwardAdd, Params: api.ForwardParams{Host: h.Name, Spec: "gpg-agent"}, Done: "forward gpg-agent to " + h.Title()}
	}
	children = append(children, gpg)
	if by := gpgUsedBy(h); by != "" {
		children = append(children, action(id+":gpg-claim", "Use this machine's gpg keys (in use by "+by+")", gpgClaimAction(h)))
	}
	if h.AdHoc {
		children = append(children, action(id+":forget", "Forget this host", &Action{Method: api.MethodHostRemove, Params: api.HostParams{Name: h.Name}, Done: "forget " + h.Title()}))
	}
	return Item{ID: id, Title: title, Enabled: true, Children: children}
}

// forwardItem is a forward with its submenu: what it does, whether its
// target answers, and copy, open and remove.
func forwardItem(id, host string, f api.ForwardStatus) Item {
	title := stateMark[f.State] + " " + f.Spec
	if f.Label != "" {
		title = stateMark[f.State] + " " + f.Label + " (" + f.Spec + ")"
	}
	switch {
	case f.Error != "":
		title += " — " + f.Error
	case f.UsedBy != "" && f.State == api.StateUp:
		title = "◑ " + strings.TrimPrefix(title, stateMark[f.State]+" ") + " — standing by, in use by " + f.UsedBy
	case f.Target == "unreachable":
		title += " — ⚠ target unreachable"
	case f.Address != "":
		title += " — " + f.Address
	case f.AllocatedPort != 0:
		title += fmt.Sprintf(" — remote port %d", f.AllocatedPort)
	}
	it := Item{ID: id + ":fwd:" + f.Spec, Title: title, Enabled: true}
	if f.Description != "" {
		it.Children = append(it.Children, label(it.ID+":what", f.Description))
	}
	if f.TargetError != "" {
		it.Children = append(it.Children, label(it.ID+":target", "⚠ "+f.TargetError))
	}
	if f.Address != "" {
		it.Children = append(it.Children, action(it.ID+":copy", "Copy address ("+f.Address+")", &Action{Local: localCopy, Arg: f.Address}))
		if !strings.HasPrefix(f.Address, "/") && !strings.HasPrefix(f.Spec, "D:") {
			it.Children = append(it.Children, action(it.ID+":open", "Open in browser", &Action{Local: localOpenURL, Arg: "http://" + f.Address}))
		}
	}
	if f.AdHoc {
		it.Children = append(it.Children, action(it.ID+":rm", "Remove", &Action{
			Method: api.MethodForwardRemove, Params: api.ForwardParams{Host: host, Spec: f.Spec}, Done: "remove " + f.Spec,
		}))
	}
	if len(it.Children) == 0 {
		it.Enabled = false
	}
	return it
}

// addKinds are the things the Add submenu offers, in the terminal UI's
// order; HOST is replaced by the host's title. local marks those offered on
// the local host.
var addKinds = []struct {
	kind, title string
	local       bool
}{
	{addLocal, "Forward a local port to a port on HOST…", false},
	{addRemote, "Forward a port on HOST to a port here…", false},
	{addToHost, "Forward a local port to a machine on HOST's network…", false},
	{addFromLAN, "Forward a port on HOST to a machine on my network…", false},
	{addSOCKS, "SOCKS proxy here, connecting out from HOST…", false},
	{addReverseSOCKS, "SOCKS proxy on HOST, connecting out from here…", false},
	{addHTTP, "HTTP proxy here, connecting out from HOST…", false},
	{addKube, "Kubernetes service or pod…", true},
	{addMountHere, "Mount a directory from HOST here…", false},
	{addMountThere, "Mount a local directory on HOST…", false},
	{addPVC, "Mount a Kubernetes claim (PVC)…", true},
	{addSpec, "Type a forward spec…", true},
}

// What the Add submenu's entries ask for (Action.Arg with localAdd).
const (
	addLocal        = "local"
	addRemote       = "remote"
	addToHost       = "tohost"
	addFromLAN      = "fromlan"
	addSOCKS        = "socks"
	addReverseSOCKS = "rsocks"
	addHTTP         = "http"
	addKube         = "k8s"
	addMountHere    = "mount-here"
	addMountThere   = "mount-there"
	addPVC          = "pvc"
	addSpec         = "spec"
)

// addItem is the host's Add submenu: every kind of forward and mount, the
// USB devices that could be shared with it, and the terminal UI.
func addItem(id string, h api.HostStatus, usb []api.USBDevice) Item {
	add := Item{ID: id + ":add", Title: "Add", Enabled: true}
	for _, k := range addKinds {
		if h.Local && !k.local {
			continue
		}
		title := strings.ReplaceAll(k.title, "HOST", h.Title())
		add.Children = append(add.Children, action(add.ID+":"+k.kind, title, &Action{Local: localAdd, Host: h.Name, Arg: k.kind}))
	}
	if !h.Local {
		var free []api.USBDevice
		for _, d := range usb {
			if d.Host == "" {
				free = append(free, d)
			}
		}
		if len(free) > 0 {
			share := Item{ID: add.ID + ":usb", Title: "Share a USB device", Enabled: true}
			for _, d := range free {
				share.Children = append(share.Children, action(share.ID+":"+d.BusID, d.Title()+" at "+d.BusID, &Action{
					Method: api.MethodUSBAttach, Params: api.USBParams{Host: h.Name, Device: d.BusID}, Done: "share " + d.Title() + " with " + h.Title(),
				}))
			}
			add.Children = append(add.Children, share)
		}
	}
	add.Children = append(add.Children, separator(add.ID+":sep"),
		action(add.ID+":tui", "More in the terminal UI…", &Action{Local: localOpenTUI, Host: h.Name, Arg: "--add"}))
	return add
}

// recentItems adds again, one click each, a forward added or claim mounted
// lately on h; ones there now are left out.
func recentItems(id string, h api.HostStatus) []Item {
	var items []Item
	for _, r := range h.RecentForwards {
		if slices.ContainsFunc(h.Forwards, func(f api.ForwardStatus) bool { return f.Spec == r.Spec }) {
			continue
		}
		title, in := r.Spec, r.Spec
		if r.Label != "" {
			title, in = r.Label+" ("+r.Spec+")", r.Label+"="+r.Spec
		}
		items = append(items, action(id+":recent:fwd:"+r.Spec, "Forward "+title, &Action{
			Method: api.MethodForwardAdd, Params: api.ForwardParams{Host: h.Name, Spec: in}, Done: "add " + r.Spec,
		}))
	}
	for _, r := range h.RecentPVCs {
		mounted := false
		for _, m := range h.Mounts {
			if k := m.Kube; k != nil && k.Context == r.Context && k.Namespace == r.Namespace && k.PVC == r.PVC {
				mounted = true
			}
		}
		if mounted {
			continue
		}
		ref := r.Namespace + "/" + r.PVC
		if r.Context != "" {
			ref = r.Context + "/" + ref
		}
		title := "Mount " + ref + " → " + r.Local
		if r.ReadOnly {
			title += " (read-only)"
		}
		k := r.KubeMount
		p := api.MountParams{Host: h.Name, Direction: "pvc-to-local", Local: r.Local, Kube: &k}
		if r.ReadOnly {
			p.Options = []string{"ro"}
		}
		items = append(items, action(id+":recent:"+ref, title, &Action{Method: api.MethodMountAdd, Params: p, Done: "mount " + ref}))
	}
	return items
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
		// Where it's shared now, if anywhere; it can be moved from there
		// unless a profile put it there.
		holder, movable := "", false
		for _, h := range st.Hosts {
			if u, shared := sharedWith(h, d); shared {
				holder, movable = h.Name, u.AdHoc
			}
		}
		var children []Item
		for _, h := range st.Hosts {
			u, shared := sharedWith(h, d)
			it := Item{ID: id + ":" + h.Name, Title: "Share with " + h.Title(), Enabled: true, Checkable: true, Checked: shared}
			switch {
			case shared && !u.AdHoc:
				it.Title += " (profile " + strings.Join(u.Profiles, ", ") + ")"
				it.Enabled = false
			case shared:
				it.Action = &Action{Method: api.MethodUSBDetach, Params: api.USBParams{Host: h.Name, Device: u.Device}, Done: "stop sharing " + d.Title() + " with " + h.Title()}
			case holder != "" && movable:
				it.Action = &Action{Method: api.MethodUSBAttach, Params: api.USBParams{Host: h.Name, Device: d.BusID, Move: true}, Done: "move " + d.Title() + " to " + h.Title()}
			case holder != "" || d.Host != "":
				it.Enabled = false // a profile has it elsewhere
			default:
				it.Action = &Action{Method: api.MethodUSBAttach, Params: api.USBParams{Host: h.Name, Device: d.BusID}, Done: "share " + d.Title() + " with " + h.Title()}
			}
			if shared {
				title = stateMark[u.State] + " " + d.Title() + " — " + h.Title()
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
