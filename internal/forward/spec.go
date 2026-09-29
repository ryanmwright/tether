// Package forward parses port and socket forward specs.
//
// Specs use ssh's own -L/-R/-D syntax behind a kind prefix:
//
//	L:[bind:]port:host:hostport    L:[bind:]port:/remote.sock
//	L:/local.sock:host:hostport    L:/local.sock:/remote.sock
//	R:... (same forms; the listen side is on the remote)
//	D:[bind:]port                  SOCKS proxy here, out through the remote
//	R:[bind:]port                  SOCKS proxy on the remote, out through here
//
// and two kinds of tether's own:
//
//	H:[bind:]port                  HTTP (and SOCKS5) proxy here, out through the remote
//	K:[bind:]port:[context/]namespace/kind/name:port
//	                               a Kubernetes service or pod, via kubectl on the remote
//
// A local port of 0 means any free port. IPv6 addresses are written in
// brackets, e.g. L:[::1]:8080:[fd00::5]:80.
package forward

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

type Kind byte

const (
	Local   Kind = 'L'
	Remote  Kind = 'R'
	Dynamic Kind = 'D'
	HTTP    Kind = 'H' // tether's HTTP proxy, backed by a hidden dynamic forward
	Kube    Kind = 'K' // kubectl port-forward on the remote, brought here
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
	// Target is unset for SOCKS and HTTP proxies. For Kube, Host is the
	// reference "[context/]namespace/kind/name" and Port the port there.
	Target Endpoint
}

// String is the canonical form; equal forwards have equal strings.
func (s Spec) String() string { return string(s.Kind) + ":" + s.Arg() }

// Flag is the ssh option for this kind: -L, -R or -D.
func (s Spec) Flag() string { return "-" + string(s.Kind) }

// Arg is the argument to Flag.
func (s Spec) Arg() string {
	switch {
	case s.IsProxy():
		return s.Listen.String()
	case s.Kind == Kube:
		return s.Listen.String() + ":" + s.Target.Host + ":" + strconv.Itoa(s.Target.Port)
	}
	return s.Listen.String() + ":" + s.Target.String()
}

// ListensLocally reports whether the listening side is on this machine.
func (s Spec) ListensLocally() bool { return s.Kind != Remote }

// IsProxy reports whether s is a proxy (SOCKS or HTTP) rather than a
// forward to one target.
func (s Spec) IsProxy() bool {
	return s.Kind == Dynamic || s.Kind == HTTP || s.Kind == Remote && s.IsReverseSOCKS()
}

// IsReverseSOCKS reports whether s is a SOCKS proxy on the remote that
// connects out from this machine (ssh -R with no target).
func (s Spec) IsReverseSOCKS() bool {
	return s.Kind == Remote && s.Target == (Endpoint{})
}

// AutoPort reports whether s listens here on a port chosen when it's added.
func (s Spec) AutoPort() bool {
	return s.ListensLocally() && !s.Listen.IsSocket() && s.Listen.Port == 0
}

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
		return Spec{}, fmt.Errorf("must start with a kind: L, R, D, H or K")
	}
	spec := Spec{Kind: Kind(unicode.ToUpper(rune(kindStr[0])))}
	switch spec.Kind {
	case Local, Remote, Dynamic, HTTP:
	case Kube:
		return parseKube(rest)
	default:
		return Spec{}, fmt.Errorf("unknown kind %q, want L, R, D, H or K", kindStr)
	}
	toks, err := split(rest)
	if err != nil {
		return Spec{}, err
	}

	listenToks := toks
	// R:[bind:]port, without a target, is a reverse SOCKS proxy.
	reverseSOCKS := spec.Kind == Remote && (len(toks) == 1 && !isPath(toks[0]) || len(toks) == 2 && !isPath(toks[1]) && isPort(toks[1]))
	if spec.Kind != Dynamic && spec.Kind != HTTP && !reverseSOCKS {
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
		if spec.Kind == Dynamic || spec.Kind == HTTP {
			return Spec{}, fmt.Errorf("proxies can't listen on a Unix socket")
		}
		spec.Listen = Endpoint{Socket: listenToks[0]}
	case len(listenToks) == 1:
		spec.Listen, err = hostPort("", listenToks[0], true)
	case len(listenToks) == 2:
		spec.Listen, err = hostPort(listenToks[0], listenToks[1], true)
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

func isPort(tok string) bool {
	p, err := strconv.Atoi(tok)
	return err == nil && p >= 0 && p <= 65535
}

// kubeKinds are the kinds of Kubernetes object a forward can reach, by the
// names kubectl accepts, with the canonical name.
var kubeKinds = map[string]string{
	"svc": "svc", "service": "svc", "services": "svc",
	"pod": "pod", "pods": "pod", "po": "pod",
	"deploy": "deploy", "deployment": "deploy", "deployments": "deploy",
	"sts": "sts", "statefulset": "sts", "statefulsets": "sts",
}

// parseKube parses "[bind:]port:[context/]namespace/kind/name:port". The
// context may itself contain colons and slashes (EKS ARNs), so the listen
// side is taken from the front and the port from the back.
func parseKube(rest string) (Spec, error) {
	spec := Spec{Kind: Kube}
	// The listen side is "port" or "bind:port" (bind may be bracketed IPv6).
	var listen []string
	for len(listen) < 2 && rest != "" {
		var tok string
		if rest[0] == '[' {
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return spec, fmt.Errorf("unterminated '['")
			}
			tok, rest = rest[1:end], strings.TrimPrefix(rest[end+1:], ":")
		} else {
			tok, rest, _ = strings.Cut(rest, ":")
		}
		listen = append(listen, tok)
		if isPort(tok) {
			break
		}
	}
	var err error
	switch len(listen) {
	case 1:
		spec.Listen, err = hostPort("", listen[0], true)
	case 2:
		spec.Listen, err = hostPort(listen[0], listen[1], true)
	}
	if len(listen) == 0 || err != nil || !isPort(listen[len(listen)-1]) {
		return spec, fmt.Errorf("expected [bind:]port:[CONTEXT/]NAMESPACE/KIND/NAME:PORT")
	}
	i := strings.LastIndexByte(rest, ':')
	if i < 0 {
		return spec, fmt.Errorf("missing target port: [CONTEXT/]NAMESPACE/KIND/NAME:PORT")
	}
	ref, portStr := rest[:i], rest[i+1:]
	k, err := ParseKubeRef(ref)
	if err != nil {
		return spec, err
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 || p > 65535 {
		return spec, fmt.Errorf("invalid port %q", portStr)
	}
	spec.Target = Endpoint{Host: k.String(), Port: p}
	return spec, nil
}

// KubeRef names a Kubernetes object to forward to.
type KubeRef struct {
	Context, Namespace, Kind, Name string // Kind is svc, pod, deploy or sts
}

// String is "[context/]namespace/kind/name".
func (k KubeRef) String() string {
	s := k.Namespace + "/" + k.Kind + "/" + k.Name
	if k.Context != "" {
		s = k.Context + "/" + s
	}
	return s
}

// Object is "kind/name", as kubectl takes it.
func (k KubeRef) Object() string { return k.Kind + "/" + k.Name }

// ParseKubeRef parses "[context/]namespace/kind/name".
func ParseKubeRef(ref string) (KubeRef, error) {
	parts := strings.Split(ref, "/")
	n := len(parts)
	if n < 3 {
		return KubeRef{}, fmt.Errorf("%q: want [CONTEXT/]NAMESPACE/KIND/NAME, e.g. prod/web/svc/frontend", ref)
	}
	k := KubeRef{Context: strings.Join(parts[:n-3], "/"), Namespace: parts[n-3], Name: parts[n-1]}
	kind, ok := kubeKinds[strings.ToLower(parts[n-2])]
	if !ok {
		return KubeRef{}, fmt.Errorf("unknown kind %q: want svc, pod, deploy or sts", parts[n-2])
	}
	k.Kind = kind
	if !kubeName.MatchString(k.Namespace) || !kubeName.MatchString(k.Name) {
		return KubeRef{}, fmt.Errorf("%q: invalid namespace or name", ref)
	}
	if strings.HasPrefix(k.Context, "-") || strings.ContainsFunc(k.Context, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return KubeRef{}, fmt.Errorf("invalid context %q", k.Context)
	}
	return k, nil
}

var kubeName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// KubeRef is the object a Kube forward reaches.
func (s Spec) KubeRef() KubeRef {
	k, _ := ParseKubeRef(s.Target.Host)
	return k
}

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
