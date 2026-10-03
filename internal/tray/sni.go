package tray

import (
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

// The tray icon is a StatusNotifierItem: an object on the session bus that
// the desktop's tray (the StatusNotifierWatcher) is told about and shows,
// with its menu at menuPath.

const (
	itemPath     = dbus.ObjectPath("/StatusNotifierItem")
	itemIface    = "org.kde.StatusNotifierItem"
	watcherName  = "org.kde.StatusNotifierWatcher"
	watcherPath  = dbus.ObjectPath("/StatusNotifierWatcher")
	busInterface = "org.freedesktop.DBus"
)

type pixmap struct {
	Width, Height int32
	ARGB          []byte
}

type toolTip struct {
	IconName string
	Icon     []pixmap
	Title    string
	Text     string
}

type statusItem struct {
	conn  *dbus.Conn
	name  string
	props *prop.Properties
	log   *slog.Logger

	mu      sync.Mutex
	look    Look
	tooltip string
}

// newStatusItem exports the icon, with the menu exported on conn; Show puts
// it in the tray. onActivate runs on a left click.
func newStatusItem(conn *dbus.Conn, onActivate func(), log *slog.Logger) (*statusItem, error) {
	s := &statusItem{conn: conn, name: fmt.Sprintf("org.kde.StatusNotifierItem-%d-1", os.Getpid()), log: log}
	if err := conn.Export(itemMethods{onActivate}, itemPath, itemIface); err != nil {
		return nil, err
	}
	ro := func(v any) *prop.Prop { return &prop.Prop{Value: v, Emit: prop.EmitFalse} }
	props, err := prop.Export(conn, itemPath, prop.Map{itemIface: {
		"Category":            ro("ApplicationStatus"),
		"Id":                  ro("tether"),
		"Title":               ro("tether"),
		"Status":              ro("Active"),
		"WindowId":            ro(int32(0)),
		"IconName":            ro(""),
		"IconPixmap":          ro([]pixmap{}),
		"IconThemePath":       ro(""),
		"OverlayIconName":     ro(""),
		"OverlayIconPixmap":   ro([]pixmap{}),
		"AttentionIconName":   ro(""),
		"AttentionIconPixmap": ro([]pixmap{}),
		"AttentionMovieName":  ro(""),
		"ToolTip":             ro(toolTip{Icon: []pixmap{}}),
		"ItemIsMenu":          ro(false),
		"Menu":                ro(menuPath),
	}})
	if err != nil {
		return nil, err
	}
	s.props = props
	if err := conn.Export(introspect.Introspectable(itemIntrospection), itemPath, "org.freedesktop.DBus.Introspectable"); err != nil {
		return nil, err
	}
	return s, nil
}

// Show puts the icon in the desktop's tray, now and whenever the tray
// restarts.
func (s *statusItem) Show() error {
	conn := s.conn
	if reply, err := conn.RequestName(s.name, dbus.NameFlagDoNotQueue); err != nil {
		return err
	} else if reply != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("bus name %s is taken", s.name)
	}

	// Register again whenever a tray starts, e.g. after plasmashell restarts.
	if err := conn.AddMatchSignal(
		dbus.WithMatchSender(busInterface), dbus.WithMatchInterface(busInterface),
		dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, watcherName),
	); err != nil {
		return err
	}
	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)
	go func() {
		for sig := range signals { // closed with the connection
			if sig.Name != busInterface+".NameOwnerChanged" || len(sig.Body) != 3 {
				continue
			}
			if name, _ := sig.Body[0].(string); name != watcherName {
				continue
			}
			if owner, _ := sig.Body[2].(string); owner != "" {
				s.register()
			}
		}
	}()
	s.register()
	return nil
}

func (s *statusItem) register() {
	err := s.conn.Object(watcherName, watcherPath).Call(watcherName+".RegisterStatusNotifierItem", 0, s.name).Err
	if err != nil {
		s.log.Warn("no system tray to show the icon in yet", "err", err)
	}
}

// SetIcon shows the icon for look, telling the tray only if it changed.
func (s *statusItem) SetIcon(look Look) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if look == s.look {
		return
	}
	s.look = look
	w, h, argb := IconPixmap(look)
	s.props.SetMust(itemIface, "IconPixmap", []pixmap{{int32(w), int32(h), argb}})
	s.emit("NewIcon")
}

// SetToolTip sets the text shown on hovering the icon.
func (s *statusItem) SetToolTip(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if text == s.tooltip {
		return
	}
	s.tooltip = text
	s.props.SetMust(itemIface, "ToolTip", toolTip{Icon: []pixmap{}, Title: text})
	s.emit("NewToolTip")
}

func (s *statusItem) emit(signal string) {
	if err := s.conn.Emit(itemPath, itemIface+"."+signal); err != nil {
		s.log.Debug("tray signal failed", "signal", signal, "err", err)
	}
}

// itemMethods are the icon's D-Bus methods.
type itemMethods struct{ onActivate func() }

func (im itemMethods) Activate(x, y int32) *dbus.Error {
	im.onActivate()
	return nil
}

func (itemMethods) SecondaryActivate(x, y int32) *dbus.Error           { return nil }
func (itemMethods) ContextMenu(x, y int32) *dbus.Error                 { return nil }
func (itemMethods) Scroll(delta int32, orientation string) *dbus.Error { return nil }
func (itemMethods) ProvideXdgActivationToken(token string) *dbus.Error { return nil }

const itemIntrospection = `<node>
  <interface name="org.kde.StatusNotifierItem">
    <property name="Category" type="s" access="read"/>
    <property name="Id" type="s" access="read"/>
    <property name="Title" type="s" access="read"/>
    <property name="Status" type="s" access="read"/>
    <property name="WindowId" type="i" access="read"/>
    <property name="IconName" type="s" access="read"/>
    <property name="IconPixmap" type="a(iiay)" access="read"/>
    <property name="IconThemePath" type="s" access="read"/>
    <property name="OverlayIconName" type="s" access="read"/>
    <property name="OverlayIconPixmap" type="a(iiay)" access="read"/>
    <property name="AttentionIconName" type="s" access="read"/>
    <property name="AttentionIconPixmap" type="a(iiay)" access="read"/>
    <property name="AttentionMovieName" type="s" access="read"/>
    <property name="ToolTip" type="(sa(iiay)ss)" access="read"/>
    <property name="ItemIsMenu" type="b" access="read"/>
    <property name="Menu" type="o" access="read"/>
    <method name="Activate">
      <arg type="i" name="x" direction="in"/>
      <arg type="i" name="y" direction="in"/>
    </method>
    <method name="SecondaryActivate">
      <arg type="i" name="x" direction="in"/>
      <arg type="i" name="y" direction="in"/>
    </method>
    <method name="ContextMenu">
      <arg type="i" name="x" direction="in"/>
      <arg type="i" name="y" direction="in"/>
    </method>
    <method name="Scroll">
      <arg type="i" name="delta" direction="in"/>
      <arg type="s" name="orientation" direction="in"/>
    </method>
    <method name="ProvideXdgActivationToken">
      <arg type="s" name="token" direction="in"/>
    </method>
    <signal name="NewTitle"/>
    <signal name="NewIcon"/>
    <signal name="NewAttentionIcon"/>
    <signal name="NewOverlayIcon"/>
    <signal name="NewToolTip"/>
    <signal name="NewStatus">
      <arg type="s" name="status"/>
    </signal>
  </interface>
` + introspect.IntrospectDataString + prop.IntrospectDataString + `</node>`
