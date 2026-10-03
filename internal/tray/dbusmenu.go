package tray

import (
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

// The tray's menu, exported as com.canonical.dbusmenu, which is how a
// StatusNotifierItem shows one. Writing it ourselves, rather than through a
// tray library, is what makes KDE's copy of it behave:
//
//   - Each item's ID comes from Item.ID, so it stays the same across status
//     changes and KDE keeps its copy of the item and its submenu.
//   - A change to an item's label, check or enabled state is sent as just
//     that (ItemsPropertiesUpdated). A changed list of items is sent for the
//     one menu it's in (LayoutUpdated). KDE handles the latter by taking
//     every entry out of that menu and putting it back, which loses the
//     entry under the mouse and the submenu about to open there, so while a
//     menu is open, changes to the lists it shows wait until it closes.
//   - KDE fetches a submenu only once it's hovered, too late for that hover
//     to open it. So after each fetch, the menu tells it that the submenus it
//     has just seen, but not their contents, changed, and KDE fetches those
//     too: the whole menu is loaded before anything is hovered.

const (
	menuPath  = dbus.ObjectPath("/MenuBar")
	menuIface = "com.canonical.dbusmenu"

	// nudgeDelay lets the reply to a fetch go out before the nudges for the
	// submenus it showed.
	nudgeDelay = 20 * time.Millisecond
	// holdLimit is the longest a change to an open menu's entries waits for
	// it to close, in case the desktop doesn't say it closed.
	holdLimit = 10 * time.Second
)

// kind is what an item is; KDE takes it only when it first sees the item,
// so an item that changes kind gets a new ID.
type kind uint8

const (
	kindSeparator kind = iota
	kindPlain
	kindCheck
	kindMenu
	kindCheckMenu
)

func kindOf(it Item) kind {
	switch {
	case it.Separator:
		return kindSeparator
	case len(it.Children) > 0 && it.Checkable:
		return kindCheckMenu
	case len(it.Children) > 0:
		return kindMenu
	case it.Checkable:
		return kindCheck
	}
	return kindPlain
}

// menuNode is an item as exported; the root, ID 0, holds the top level.
type menuNode struct {
	props    map[string]dbus.Variant
	children []int32
	action   *Action
	fetched  bool // the desktop has fetched the current children
}

// menuLayout is an item with its children, as GetLayout returns it.
type menuLayout struct {
	ID       int32
	Props    map[string]dbus.Variant
	Children []dbus.Variant // menuLayout
}

type menuItemProps struct {
	ID    int32
	Props map[string]dbus.Variant
}

type menuItemPropNames struct {
	ID    int32
	Names []string
}

type menuEvent struct {
	ID        int32
	EventID   string
	Data      dbus.Variant
	Timestamp uint32
}

type menu struct {
	conn    *dbus.Conn
	onClick func(*Action)
	log     *slog.Logger

	mu    sync.Mutex
	ids   map[string]int32 // by item key; never reused for another item
	next  int32
	nodes map[int32]*menuNode
	rev   uint32
	open  map[int32]bool // menus shown now
	held  []Item         // the latest items, while an open menu's entries would change
	timer *time.Timer    // holdLimit for held
}

// newMenu exports an empty menu on conn. onClick runs the action of a
// clicked item.
func newMenu(conn *dbus.Conn, onClick func(*Action), log *slog.Logger) (*menu, error) {
	m := &menu{
		conn: conn, onClick: onClick, log: log,
		ids:   map[string]int32{},
		nodes: map[int32]*menuNode{0: rootNode(nil)},
		open:  map[int32]bool{},
	}
	if err := conn.Export(menuMethods{m}, menuPath, menuIface); err != nil {
		return nil, err
	}
	ro := func(v any) *prop.Prop { return &prop.Prop{Value: v, Emit: prop.EmitFalse} }
	if _, err := prop.Export(conn, menuPath, prop.Map{menuIface: {
		"Version":       ro(uint32(3)),
		"TextDirection": ro("ltr"),
		"Status":        ro("normal"),
		"IconThemePath": ro([]string{}),
	}}); err != nil {
		return nil, err
	}
	if err := conn.Export(introspect.Introspectable(menuIntrospection), menuPath, "org.freedesktop.DBus.Introspectable"); err != nil {
		return nil, err
	}
	return m, nil
}

func rootNode(children []int32) *menuNode {
	return &menuNode{props: map[string]dbus.Variant{"children-display": dbus.MakeVariant("submenu")}, children: children}
}

// Update shows items, telling the desktop only what changed.
func (m *menu) Update(items []Item) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.update(items)
}

func (m *menu) update(items []Item) {
	next := m.build(items)
	var changed []int32 // menus whose entries changed
	hold := false
	for id, n := range next {
		if old, ok := m.nodes[id]; ok && !slices.Equal(old.children, n.children) {
			changed = append(changed, id)
			hold = hold || m.open[id]
		}
	}
	if hold {
		// Change the open menus' entries in place now; adding and removing
		// entries waits.
		m.held = items
		if m.timer == nil {
			m.timer = time.AfterFunc(holdLimit, m.holdExpired)
		}
		shown := make(map[int32]*menuNode, len(m.nodes))
		for id, old := range m.nodes {
			shown[id] = old
			if n, ok := next[id]; ok {
				shown[id] = &menuNode{props: n.props, action: n.action, children: old.children, fetched: old.fetched}
			}
		}
		next, changed = shown, nil
	} else {
		m.held = nil
		if m.timer != nil {
			m.timer.Stop()
			m.timer = nil
		}
	}

	var updated []menuItemProps
	for id, n := range next {
		old, ok := m.nodes[id]
		if !ok {
			continue
		}
		n.fetched = old.fetched && slices.Equal(old.children, n.children)
		if !sameProps(old.props, n.props) {
			updated = append(updated, menuItemProps{id, n.props})
		}
	}
	m.nodes = next
	if len(updated) > 0 {
		m.emit("ItemsPropertiesUpdated", updated, []menuItemPropNames{})
	}
	if len(changed) > 0 {
		m.rev++
		slices.Sort(changed)
		for _, id := range changed {
			m.emit("LayoutUpdated", m.rev, id)
		}
	}
}

// build makes the nodes for items, giving each the ID its key had before.
func (m *menu) build(items []Item) map[int32]*menuNode {
	nodes := map[int32]*menuNode{}
	seen := map[string]int{}
	var add func([]Item) []int32
	add = func(items []Item) []int32 {
		var ids []int32
		for _, it := range items {
			key := it.ID + "\x00" + strconv.Itoa(int(kindOf(it)))
			if seen[key]++; seen[key] > 1 {
				key += "\x00" + strconv.Itoa(seen[key]) // a duplicate ID
			}
			id, ok := m.ids[key]
			if !ok {
				m.next++
				id = m.next
				m.ids[key] = id
			}
			nodes[id] = &menuNode{props: itemProps(it), action: it.Action, children: add(it.Children)}
			ids = append(ids, id)
		}
		return ids
	}
	nodes[0] = rootNode(add(items))
	return nodes
}

// itemProps is an item's dbusmenu properties. An item always has the same
// ones, so none is ever removed.
func itemProps(it Item) map[string]dbus.Variant {
	if it.Separator {
		return map[string]dbus.Variant{"type": dbus.MakeVariant("separator")}
	}
	p := map[string]dbus.Variant{
		// An underscore marks the access key; two show one.
		"label":   dbus.MakeVariant(strings.ReplaceAll(it.Title, "_", "__")),
		"enabled": dbus.MakeVariant(it.Enabled),
	}
	if it.Checkable {
		state := int32(0)
		if it.Checked {
			state = 1
		}
		p["toggle-type"] = dbus.MakeVariant("checkmark")
		p["toggle-state"] = dbus.MakeVariant(state)
	}
	if len(it.Children) > 0 {
		p["children-display"] = dbus.MakeVariant("submenu")
	}
	return p
}

func sameProps(a, b map[string]dbus.Variant) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v.Value() != w.Value() {
			return false
		}
	}
	return true
}

func (m *menu) emit(signal string, args ...any) {
	if err := m.conn.Emit(menuPath, menuIface+"."+signal, args...); err != nil {
		m.log.Debug("menu signal failed", "signal", signal, "err", err)
	}
}

// layout is n as GetLayout returns it, depth levels deep (all if negative).
// It notes in nudge the submenus at the bottom whose entries the desktop
// hasn't fetched.
func (m *menu) layout(id int32, n *menuNode, depth int32, names []string, nudge *[]int32) menuLayout {
	l := menuLayout{ID: id, Props: filterProps(n.props, names), Children: []dbus.Variant{}}
	if depth == 0 {
		if len(n.children) > 0 && !n.fetched {
			*nudge = append(*nudge, id)
		}
		return l
	}
	n.fetched = true
	for _, c := range n.children {
		l.Children = append(l.Children, dbus.MakeVariant(m.layout(c, m.nodes[c], depth-1, names, nudge)))
	}
	return l
}

func filterProps(props map[string]dbus.Variant, names []string) map[string]dbus.Variant {
	if len(names) == 0 {
		return props
	}
	out := map[string]dbus.Variant{}
	for _, name := range names {
		if v, ok := props[name]; ok {
			out[name] = v
		}
	}
	return out
}

// nudge says the entries of submenus the desktop hasn't fetched changed, so
// it fetches them.
func (m *menu) nudge(ids []int32) {
	time.Sleep(nudgeDelay)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		if n, ok := m.nodes[id]; ok && !n.fetched {
			m.emit("LayoutUpdated", m.rev, id)
		}
	}
}

func (m *menu) event(id int32, eventID string) bool {
	m.mu.Lock()
	n, ok := m.nodes[id]
	var a *Action
	switch {
	case eventID == "closed": // also for a menu removed since
		ok = true
		delete(m.open, id)
		if len(m.open) == 0 && m.held != nil {
			m.update(m.held)
		}
	case !ok:
	case eventID == "clicked":
		a = n.action
	case eventID == "opened":
		m.open[id] = true
	}
	m.mu.Unlock()
	if a != nil {
		m.onClick(a)
	}
	return ok
}

// holdExpired shows held items when the desktop hasn't said the menu
// closed: it may not say so at all.
func (m *menu) holdExpired() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.timer = nil
	clear(m.open)
	if m.held != nil {
		m.update(m.held)
	}
}

// menuMethods are the menu's D-Bus methods.
type menuMethods struct{ m *menu }

func noItem(id int32) *dbus.Error {
	return dbus.MakeFailedError(fmt.Errorf("no menu item %d", id))
}

func (mm menuMethods) GetLayout(parent, depth int32, names []string) (uint32, menuLayout, *dbus.Error) {
	m := mm.m
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[parent]
	if !ok {
		return 0, menuLayout{}, noItem(parent)
	}
	var nudge []int32
	l := m.layout(parent, n, depth, names, &nudge)
	if len(nudge) > 0 {
		go m.nudge(nudge)
	}
	return m.rev, l, nil
}

func (mm menuMethods) GetGroupProperties(ids []int32, names []string) ([]menuItemProps, *dbus.Error) {
	m := mm.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(ids) == 0 {
		for id := range m.nodes {
			ids = append(ids, id)
		}
		slices.Sort(ids)
	}
	out := []menuItemProps{}
	for _, id := range ids {
		if n, ok := m.nodes[id]; ok {
			out = append(out, menuItemProps{id, filterProps(n.props, names)})
		}
	}
	return out, nil
}

func (mm menuMethods) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	m := mm.m
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok {
		return dbus.Variant{}, noItem(id)
	}
	v, ok := n.props[name]
	if !ok {
		return dbus.Variant{}, dbus.MakeFailedError(fmt.Errorf("menu item %d has no property %q", id, name))
	}
	return v, nil
}

func (mm menuMethods) Event(id int32, eventID string, data dbus.Variant, timestamp uint32) *dbus.Error {
	if !mm.m.event(id, eventID) {
		return noItem(id)
	}
	return nil
}

func (mm menuMethods) EventGroup(events []menuEvent) ([]int32, *dbus.Error) {
	errs := []int32{}
	for _, e := range events {
		if !mm.m.event(e.ID, e.EventID) {
			errs = append(errs, e.ID)
		}
	}
	return errs, nil
}

// AboutToShow reports whether the desktop's copy of a submenu is out of
// date. It normally isn't: changes are announced, and submenus fetched
// ahead.
func (mm menuMethods) AboutToShow(id int32) (bool, *dbus.Error) {
	m := mm.m
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok {
		return false, noItem(id)
	}
	return !n.fetched, nil
}

func (mm menuMethods) AboutToShowGroup(ids []int32) ([]int32, []int32, *dbus.Error) {
	m := mm.m
	m.mu.Lock()
	defer m.mu.Unlock()
	update, errs := []int32{}, []int32{}
	for _, id := range ids {
		n, ok := m.nodes[id]
		switch {
		case !ok:
			errs = append(errs, id)
		case !n.fetched:
			update = append(update, id)
		}
	}
	return update, errs, nil
}

const menuIntrospection = `<node>
  <interface name="com.canonical.dbusmenu">
    <property name="Version" type="u" access="read"/>
    <property name="TextDirection" type="s" access="read"/>
    <property name="Status" type="s" access="read"/>
    <property name="IconThemePath" type="as" access="read"/>
    <method name="GetLayout">
      <arg type="i" name="parentId" direction="in"/>
      <arg type="i" name="recursionDepth" direction="in"/>
      <arg type="as" name="propertyNames" direction="in"/>
      <arg type="u" name="revision" direction="out"/>
      <arg type="(ia{sv}av)" name="layout" direction="out"/>
    </method>
    <method name="GetGroupProperties">
      <arg type="ai" name="ids" direction="in"/>
      <arg type="as" name="propertyNames" direction="in"/>
      <arg type="a(ia{sv})" name="properties" direction="out"/>
    </method>
    <method name="GetProperty">
      <arg type="i" name="id" direction="in"/>
      <arg type="s" name="name" direction="in"/>
      <arg type="v" name="value" direction="out"/>
    </method>
    <method name="Event">
      <arg type="i" name="id" direction="in"/>
      <arg type="s" name="eventId" direction="in"/>
      <arg type="v" name="data" direction="in"/>
      <arg type="u" name="timestamp" direction="in"/>
    </method>
    <method name="EventGroup">
      <arg type="a(isvu)" name="events" direction="in"/>
      <arg type="ai" name="idErrors" direction="out"/>
    </method>
    <method name="AboutToShow">
      <arg type="i" name="id" direction="in"/>
      <arg type="b" name="needUpdate" direction="out"/>
    </method>
    <method name="AboutToShowGroup">
      <arg type="ai" name="ids" direction="in"/>
      <arg type="ai" name="updatesNeeded" direction="out"/>
      <arg type="ai" name="idErrors" direction="out"/>
    </method>
    <signal name="ItemsPropertiesUpdated">
      <arg type="a(ia{sv})" name="updatedProps"/>
      <arg type="a(ias)" name="removedProps"/>
    </signal>
    <signal name="LayoutUpdated">
      <arg type="u" name="revision"/>
      <arg type="i" name="parent"/>
    </signal>
    <signal name="ItemActivationRequested">
      <arg type="i" name="id"/>
      <arg type="u" name="timestamp"/>
    </signal>
  </interface>
` + introspect.IntrospectDataString + prop.IntrospectDataString + `</node>`
