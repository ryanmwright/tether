package tray

import "fyne.io/systray"

// The tray protocol (dbusmenu) lets an item be relabelled, enabled, checked
// and hidden in place, but adding or removing one means rebuilding the menu
// with new item IDs. KDE fetches a submenu with a new ID only once it's
// hovered, too late for that hover to open it, so after every rebuild each
// submenu needs a second hover. The tray therefore keeps slots, made once,
// and shows each menu by fitting its items into them and hiding the slots it
// doesn't need. It rebuilds only when the items don't fit, and then with room
// for what was shown before too, so going back doesn't rebuild again.

// kind is what a slot can show; the protocol can't change it later.
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

// slot is one menu entry as made in the tray.
type slot struct {
	kind     kind
	children []*slot

	// Set by the tray once the slot is made.
	mi                        *systray.MenuItem // nil for separators
	id                        string            // the item shown; "" while hidden (guarded by tray.mu)
	title                     string
	enabled, checked, visible bool
}

// slotsFor is slots for exactly items.
func slotsFor(items []Item) []*slot {
	var slots []*slot
	for _, it := range items {
		slots = append(slots, &slot{kind: kindOf(it), children: slotsFor(it.Children)})
	}
	return slots
}

// fit places items in slots, in order, returning the index of each item's
// slot, or false if they don't fit. An item needs a slot of its kind whose
// own slots fit its children. Unused slots are hidden, but separators can't
// be, so one may be left over only where Qt's menus drop it anyway: before
// everything shown, after it, or next to another separator.
func fit(slots []*slot, items []Item) ([]int, bool) {
	n, m := len(items), len(slots)
	match := make([][]bool, n)
	for i := range items {
		match[i] = make([]bool, m)
		for j, s := range slots {
			if kindOf(items[i]) == s.kind {
				_, ok := fit(s.children, items[i].Children)
				match[i][j] = ok
			}
		}
	}
	// ok[i][j][sep]: items[i:] fit slots[j:], sep meaning the last entry
	// shown was a separator, or nothing has been shown yet.
	ok := make([][][2]bool, n+1)
	for i := range ok {
		ok[i] = make([][2]bool, m+1)
	}
	ok[n][m] = [2]bool{true, true}
	for i := n; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			for sep := range 2 {
				if i == n {
					ok[i][j][sep] = true // what's left is hidden, or trailing separators
					continue
				}
				v := match[i][j] && ok[i+1][j+1][b2i(items[i].Separator)]
				if slots[j].kind == kindSeparator {
					v = v || sep == 1 && ok[i][j+1][1]
				} else {
					v = v || ok[i][j+1][sep]
				}
				ok[i][j][sep] = v
			}
		}
	}
	if !ok[0][0][1] {
		return nil, false
	}
	assign := make([]int, n)
	for i, j := 0, 0; i < n; j++ {
		if match[i][j] && ok[i+1][j+1][b2i(items[i].Separator)] {
			assign[i] = j
			i++
		}
	}
	return assign, true
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// merge is new slots that fit what slots did and items too: slots, plus
// ones for the items that have no slot of their kind.
func merge(slots []*slot, items []Item) []*slot {
	n, m := len(items), len(slots)
	// common[i][j]: the most of items[i:] that can share slots[j:].
	common := make([][]int, n+1)
	for i := range common {
		common[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			common[i][j] = max(common[i+1][j], common[i][j+1])
			if kindOf(items[i]) == slots[j].kind {
				common[i][j] = max(common[i][j], common[i+1][j+1]+1)
			}
		}
	}
	var out []*slot
	for i, j := 0, 0; i < n || j < m; {
		switch {
		case i < n && j < m && kindOf(items[i]) == slots[j].kind && common[i][j] == common[i+1][j+1]+1:
			out = append(out, &slot{kind: slots[j].kind, children: merge(slots[j].children, items[i].Children)})
			i, j = i+1, j+1
		case j < m && (i == n || common[i][j] == common[i][j+1]):
			out = append(out, &slot{kind: slots[j].kind, children: merge(slots[j].children, nil)})
			j++
		default:
			out = append(out, slotsFor(items[i:i+1])...)
			i++
		}
	}
	return out
}
