package tray

import (
	"fmt"

	"github.com/godbus/dbus/v5"

	"github.com/ryanmwright/tether/internal/api"
)

// Notice is a desktop notification about a change. Key groups notices about
// the same thing, so a newer one replaces an older one on screen.
type Notice struct {
	Key     string
	Summary string
	Body    string
	Urgent  bool
}

// Changes lists what's worth telling the user between two snapshots: hosts
// losing or regaining their connection, and forwards, mounts or USB
// devices failing.
// Things the user just asked for (connecting, going down) aren't news.
func Changes(prev, cur api.Status) []Notice {
	before := map[string]api.HostStatus{}
	for _, h := range prev.Hosts {
		before[h.Name] = h
	}
	var notices []Notice
	for _, h := range cur.Hosts {
		old, known := before[h.Name]
		if !known {
			continue
		}
		key := "host:" + h.Name
		switch {
		case h.State == api.StateError && old.State != api.StateError:
			msg := h.Error
			if old.State == api.StateUp || old.State == api.StateDegraded {
				msg = "Connection lost: " + h.Error
			}
			notices = append(notices, Notice{Key: key, Summary: h.Name + ": not connected", Body: msg + "\nRetrying in the background.", Urgent: true})
		case (h.State == api.StateUp || h.State == api.StateDegraded) && old.State == api.StateError:
			notices = append(notices, Notice{Key: key, Summary: h.Name + ": connected again"})
		}

		oldFwd := map[string]api.ForwardStatus{}
		for _, f := range old.Forwards {
			oldFwd[f.Spec] = f
		}
		for _, f := range h.Forwards {
			if f.State == api.StateError && oldFwd[f.Spec].State != api.StateError {
				notices = append(notices, Notice{Key: key + ":fwd:" + f.Spec, Summary: fmt.Sprintf("%s: %s failed", h.Name, f.Spec), Body: f.Error})
			}
		}
		oldMnt := map[string]api.MountStatus{}
		for _, m := range old.Mounts {
			oldMnt[m.Key] = m
		}
		for _, m := range h.Mounts {
			if m.State == api.StateError && oldMnt[m.Key].State != api.StateError {
				notices = append(notices, Notice{Key: key + ":mount:" + m.Key, Summary: h.Name + ": mount failed", Body: m.Key + "\n" + m.Error})
			}
		}
		oldUSB := map[string]api.USBStatus{}
		for _, u := range old.USB {
			oldUSB[u.Device] = u
		}
		for _, u := range h.USB {
			if u.State == api.StateError && oldUSB[u.Device].State != api.StateError {
				notices = append(notices, Notice{Key: key + ":usb:" + u.Device, Summary: h.Name + ": USB device " + u.Device + " failed", Body: u.Error})
			}
		}
	}
	return notices
}

// Notifier shows notices through the freedesktop notification service.
type Notifier struct {
	conn *dbus.Conn
	ids  map[string]uint32 // last notification per key, to replace it
}

func NewNotifier() (*Notifier, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	return &Notifier{conn: conn, ids: map[string]uint32{}}, nil
}

func (n *Notifier) Show(notice Notice) error {
	urgency := byte(1)
	if notice.Urgent {
		urgency = 2
	}
	obj := n.conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")
	call := obj.Call("org.freedesktop.Notifications.Notify", 0,
		"tether", n.ids[notice.Key], "network-server", notice.Summary, notice.Body,
		[]string{}, map[string]dbus.Variant{"urgency": dbus.MakeVariant(urgency)}, int32(-1))
	if call.Err != nil {
		return call.Err
	}
	var id uint32
	if err := call.Store(&id); err != nil {
		return err
	}
	n.ids[notice.Key] = id
	return nil
}

func (n *Notifier) Close() error { return n.conn.Close() }
