package tui

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// form is a short sequence of fields with a live preview of the result,
// for adding a forward or mount without knowing the syntax.
type form struct {
	title  string
	fields []formField
	focus  int
	// build turns the fields' values into what submitting does: a daemon
	// call, and a preview line saying what it will be.
	build func(v formValues) (formResult, error)
	// fromPicker makes esc go back to the Kubernetes picker, not the menu.
	fromPicker bool
}

type formField struct {
	label string
	help  string // shown next to the focused field
	input textinput.Model
	// choices makes it a choice field: all shown, picked with ←/→.
	choices []string
	choice  int
	// browse lists directories for a path field.
	browse *pathBrowser
}

type formValues []string

// get is field i's value, or def if it's empty.
func (v formValues) get(i int, def string) string {
	if s := strings.TrimSpace(v[i]); s != "" {
		return s
	}
	return def
}

type formResult struct {
	preview string // e.g. "L:5432:db:5432 — localhost:5432 here → db:5432, reached from dev"
	done    string // said when it's done
	method  string
	params  any
}

func textField(label, placeholder, value, help string) formField {
	in := textinput.New()
	in.Prompt, in.Placeholder = "", placeholder
	in.SetValue(value)
	in.SetWidth(40)
	return formField{label: label, help: help, input: in}
}

func choiceField(label, help string, choices ...string) formField {
	return formField{label: label, help: help, choices: choices}
}

// pathField is a text field for a directory, with a browser listing the
// directories matching what's typed: on host if remote, else here.
func pathField(label, placeholder, value, help string, remote bool, host string) formField {
	f := textField(label, placeholder, value, help)
	f.browse = &pathBrowser{remote: remote, host: host}
	return f
}

func (f *form) values() formValues {
	v := make(formValues, len(f.fields))
	for i, fl := range f.fields {
		if fl.choices != nil {
			v[i] = fl.choices[fl.choice]
		} else {
			v[i] = fl.input.Value()
		}
	}
	return v
}

// setFocus focuses field i, returning the cursor blink command.
func (f *form) setFocus(i int) tea.Cmd {
	for j := range f.fields {
		f.fields[j].input.Blur()
	}
	f.focus = i
	if f.fields[i].choices == nil {
		return f.fields[i].input.Focus()
	}
	return nil
}

// key handles a key press. It reports submit when enter is pressed on the
// last field (or a choice field ends the form).
func (f *form) key(msg tea.KeyPressMsg) (cmd tea.Cmd, submit bool) {
	fl := &f.fields[f.focus]
	last := f.focus == len(f.fields)-1
	if fl.browse != nil && fl.browseKey(msg.String()) {
		return nil, false
	}
	switch msg.String() {
	case "tab", "down":
		if !last {
			return f.setFocus(f.focus + 1), false
		}
		return nil, false
	case "shift+tab", "up":
		if f.focus > 0 {
			return f.setFocus(f.focus - 1), false
		}
		return nil, false
	case "enter":
		if last {
			return nil, true
		}
		return f.setFocus(f.focus + 1), false
	case "left", "right":
		if fl.choices != nil {
			step := 1
			if msg.String() == "left" {
				step = len(fl.choices) - 1
			}
			fl.choice = (fl.choice + step) % len(fl.choices)
			return nil, false
		}
	}
	if fl.choices != nil {
		return nil, false
	}
	fl.input, cmd = fl.input.Update(msg)
	return cmd, false
}

func (f *form) view() []string {
	lines := []string{styleSection.Render(f.title), ""}
	labelW := 0
	for _, fl := range f.fields {
		labelW = max(labelW, len(fl.label))
	}
	for i, fl := range f.fields {
		marker := "  "
		if i == f.focus {
			marker = "▸ "
		}
		var value string
		if fl.choices != nil {
			// Every choice shown, radio-style.
			var opts []string
			for j, c := range fl.choices {
				opt := "( ) " + c
				if j == fl.choice {
					opt = "(•) " + c
					if i == f.focus {
						opt = styleSelected.Render(opt)
					}
				}
				opts = append(opts, opt)
			}
			value = strings.Join(opts, "   ")
		} else {
			value = fl.input.View()
		}
		line := fmt.Sprintf("%s%-*s  %s", marker, labelW, fl.label, value)
		if i == f.focus && fl.help != "" {
			line += "   " + styleFaint.Render(fl.help)
		}
		lines = append(lines, line)
		if i == f.focus && fl.browse != nil {
			lines = append(lines, fl.browse.view(fl.input.Value())...)
		}
	}
	lines = append(lines, "")
	if res, err := f.build(f.values()); err != nil {
		lines = append(lines, styleWarn.Render("  "+err.Error()))
	} else {
		lines = append(lines, styleOK.Render("  → "+res.preview))
	}
	return lines
}

// portValue parses field i as a port, or def if it's empty.
func portValue(v formValues, i int, def, what string) (string, error) {
	s := v.get(i, def)
	if s == "" {
		return "", fmt.Errorf("enter %s", what)
	}
	p, err := strconv.Atoi(s)
	if err != nil || p < 0 || p > 65535 {
		return "", fmt.Errorf("%s: %q isn't a port number", what, s)
	}
	return s, nil
}

// freePort is port if nothing listens on it here, else the next free one
// after it, so suggested local ports work the first time.
func freePort(port int) int {
	for p := port; p < port+100 && p <= 65535; p++ {
		if l, err := net.Listen("tcp", net.JoinHostPort("localhost", strconv.Itoa(p))); err == nil {
			l.Close()
			return p
		}
	}
	return port
}
