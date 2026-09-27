// Package forward parses port and socket forward specs.
//
// Specs use ssh's own -L/-R/-D syntax behind a kind prefix:
//
//	L:[bind:]port:host:hostport    L:[bind:]port:/remote.sock
//	L:/local.sock:host:hostport    L:/local.sock:/remote.sock
//	R:... (same forms; the listen side is on the remote)
//	D:[bind:]port
//
// IPv6 addresses are written in brackets, e.g. L:[::1]:8080:[fd00::5]:80.
package forward

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

type Kind byte

const (
	Local   Kind = 'L'
	Remote  Kind = 'R'
	Dynamic Kind = 'D'
)

// Endpoint is either a TCP address (Host may be empty for ssh's default
// bind address) or a Unix socket path.
type Endpoint struct {
	Host   string
	Port   int
	Socket string
}

func (e Endpoint) IsSocket() bool { return e.Socket != "" }

func (e Endpoint) String() string {
	switch {
	case e.IsSocket():
		return e.Socket
	case e.Host == "":
		return strconv.Itoa(e.Port)
	case strings.Contains(e.Host, ":"):
		return "[" + e.Host + "]:" + strconv.Itoa(e.Port)
	default:
		return e.Host + ":" + strconv.Itoa(e.Port)
	}
}

type Spec struct {
	Kind   Kind
	Listen Endpoint
	Target Endpoint // unset for Dynamic
}

// String is the canonical form; equal forwards have equal strings.
func (s Spec) String() string { return string(s.Kind) + ":" + s.Arg() }

// Flag is the ssh option for this kind: -L, -R or -D.
func (s Spec) Flag() string { return "-" + string(s.Kind) }

// Arg is the argument to Flag.
func (s Spec) Arg() string {
	if s.Kind == Dynamic {
		return s.Listen.String()
	}
	return s.Listen.String() + ":" + s.Target.String()
}

// ListensLocally reports whether the listening side is on this machine.
func (s Spec) ListensLocally() bool { return s.Kind != Remote }

func Parse(s string) (Spec, error) {
	spec, err := parse(s)
	if err != nil {
		return Spec{}, fmt.Errorf("invalid forward %q: %w", s, err)
	}
	return spec, nil
}

func parse(s string) (Spec, error) {
	kindStr, rest, ok := strings.Cut(s, ":")
	if !ok || len(kindStr) != 1 {
		return Spec{}, fmt.Errorf("must start with L:, R: or D:")
	}
	spec := Spec{Kind: Kind(unicode.ToUpper(rune(kindStr[0])))}
	if spec.Kind != Local && spec.Kind != Remote && spec.Kind != Dynamic {
		return Spec{}, fmt.Errorf("unknown kind %q, want L, R or D", kindStr)
	}
	toks, err := split(rest)
	if err != nil {
		return Spec{}, err
	}

	listenToks := toks
	if spec.Kind != Dynamic {
		n := len(toks)
		switch {
		case n >= 2 && isPath(toks[n-1]):
			spec.Target = Endpoint{Socket: toks[n-1]}
			listenToks = toks[:n-1]
		case n >= 3:
			if spec.Target, err = hostPort(toks[n-2], toks[n-1], false); err != nil {
				return Spec{}, fmt.Errorf("target: %w", err)
			}
			listenToks = toks[:n-2]
		default:
			return Spec{}, fmt.Errorf("missing target")
		}
	}

	switch {
	case len(listenToks) == 1 && isPath(listenToks[0]):
		if spec.Kind == Dynamic {
			return Spec{}, fmt.Errorf("dynamic forwards can't listen on a Unix socket")
		}
		spec.Listen = Endpoint{Socket: listenToks[0]}
	case len(listenToks) == 1:
		spec.Listen, err = hostPort("", listenToks[0], spec.Kind == Remote)
	case len(listenToks) == 2:
		spec.Listen, err = hostPort(listenToks[0], listenToks[1], spec.Kind == Remote)
	default:
		return Spec{}, fmt.Errorf("expected [bind:]port for the listen side")
	}
	if err != nil {
		return Spec{}, fmt.Errorf("listen: %w", err)
	}
	return spec, nil
}

// split breaks s on colons, keeping bracketed IPv6 addresses whole and
// removing their brackets.
func split(s string) ([]string, error) {
	var toks []string
	for s != "" {
		var tok string
		if s[0] == '[' {
			end := strings.IndexByte(s, ']')
			if end < 0 {
				return nil, fmt.Errorf("unterminated '['")
			}
			tok, s = s[1:end], s[end+1:]
			if s != "" && s[0] != ':' {
				return nil, fmt.Errorf("expected ':' after ']'")
			}
			if tok == "" {
				return nil, fmt.Errorf("empty address in brackets")
			}
		} else {
			tok, _, _ = strings.Cut(s, ":")
			s = s[len(tok):]
			if tok == "" {
				return nil, fmt.Errorf("empty field")
			}
		}
		toks = append(toks, tok)
		if s != "" {
			s = s[1:] // the ':'
			if s == "" {
				return nil, fmt.Errorf("trailing ':'")
			}
		}
	}
	if len(toks) == 0 {
		return nil, fmt.Errorf("empty")
	}
	return toks, nil
}

func isPath(tok string) bool { return strings.HasPrefix(tok, "/") }

func hostPort(host, port string, allowZero bool) (Endpoint, error) {
	if strings.ContainsFunc(host, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r == '/' }) {
		return Endpoint{}, fmt.Errorf("invalid host %q", host)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 0 || p > 65535 || (p == 0 && !allowZero) {
		return Endpoint{}, fmt.Errorf("invalid port %q", port)
	}
	return Endpoint{Host: host, Port: p}, nil
}
