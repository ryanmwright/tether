package tray

import (
	"fmt"
	"sync"

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
	// Button, when Action is set, labels a button that runs it.
	Button string
	Action *Action
}

// Changes lists what's worth telling the user between two snapshots: hosts
// losing or regaining their connection, and forwards, mounts or USB
// devices failing.
// Things the user just asked for (connecting, going down) aren't news. So
// is which machine's keys gpg on a host uses, with a button to use this
// one's.
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

		if by, was := gpgUsedBy(h), gpgUsedBy(old); by != was {
			switch {
			case by != "":
				notices = append(notices, Notice{Key: key + ":gpg", Summary: "gpg on " + h.Name + ": in use by " + by,
					Body:   "gpg there uses " + by + "'s keys, not this machine's. Yours stay forwarded, ready.",
					Button: "Use this machine's keys", Action: gpgClaimAction(h.Name)})
			case gpgOn(h):
				notices = append(notices, Notice{Key: key + ":gpg", Summary: "gpg on " + h.Name + ": using this machine's keys",
					Body: "It was using " + was + "'s."})
			}
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

	mu      sync.Mutex
	ids     map[string]uint32  // last notification per key, to replace it
	actions map[uint32]*Action // what each notification's button does
}

const (
	notifications      = "org.freedesktop.Notifications"
	notificationsPath  = "/org/freedesktop/Notifications"
	notificationAction = "act"
)

// NewNotifier connects to the notification service. onAction runs the
// action of a notice whose button (or body) is clicked.
func NewNotifier(onAction func(*Action)) (*Notifier, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	n := &Notifier{conn: conn, ids: map[string]uint32{}, actions: map[uint32]*Action{}}
	if err := conn.AddMatchSignal(dbus.WithMatchInterface(notifications), dbus.WithMatchObjectPath(notificationsPath)); err == nil {
		signals := make(chan *dbus.Signal, 16)
		conn.Signal(signals)
		go func() {
			for sig := range signals { // closed with the connection
				n.signal(sig, onAction)
			}
		}()
	}
	return n, nil
}

func (n *Notifier) signal(sig *dbus.Signal, onAction func(*Action)) {
	if len(sig.Body) == 0 {
		return
	}
	id, _ := sig.Body[0].(uint32)
	n.mu.Lock()
	a := n.actions[id]
	switch sig.Name {
	case notifications + ".ActionInvoked":
	case notifications + ".NotificationClosed":
		delete(n.actions, id)
		a = nil
	default:
		a = nil
	}
	n.mu.Unlock()
	if a != nil && onAction != nil {
		onAction(a)
	}
}

func (n *Notifier) Show(notice Notice) error {
	urgency := byte(1)
	if notice.Urgent {
		urgency = 2
	}
	actions := []string{}
	if notice.Action != nil {
		// "default" is clicking the notification itself.
		actions = []string{"default", notice.Button, notificationAction, notice.Button}
	}
	n.mu.Lock()
	replaces := n.ids[notice.Key]
	n.mu.Unlock()
	obj := n.conn.Object(notifications, notificationsPath)
	call := obj.Call(notifications+".Notify", 0,
		"tether", replaces, "network-server", notice.Summary, notice.Body,
		actions, map[string]dbus.Variant{"urgency": dbus.MakeVariant(urgency)}, int32(-1))
	if call.Err != nil {
		return call.Err
	}
	var id uint32
	if err := call.Store(&id); err != nil {
		return err
	}
	n.mu.Lock()
	n.ids[notice.Key] = id
	if notice.Action != nil {
		n.actions[id] = notice.Action
	} else {
		delete(n.actions, id)
	}
	n.mu.Unlock()
	return nil
}

func (n *Notifier) Close() error { return n.conn.Close() }
