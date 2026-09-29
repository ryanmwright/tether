package forward

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// Names are shortcuts for common forwards, accepted wherever a spec is.
var Names = map[string]string{
	"socks":  "D:1080", // SOCKS proxy here, out through the remote
	"rsocks": "R:1080", // SOCKS proxy on the remote, out through here
	"http":   "H:8080", // HTTP (and SOCKS) proxy here, out through the remote
}

var labelPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]*$`)

// SplitLabel splits "label=spec" into its label and the rest. Specs never
// contain "=", so input without one has no label.
func SplitLabel(s string) (label, rest string, err error) {
	label, rest, ok := strings.Cut(strings.TrimSpace(s), "=")
	if !ok {
		return "", label, nil
	}
	label = strings.TrimSpace(label)
	if !labelPattern.MatchString(label) {
		return "", "", fmt.Errorf("invalid label %q: letters, digits, '.', '_' and '-', starting with a letter", label)
	}
	return label, strings.TrimSpace(rest), nil
}

// Expand turns a name or a shorthand into a spec; anything else (a spec, or
// a name it doesn't know) is returned as is. Shorthands are local forwards
// without the "L:":
//
//	5432                 L:5432:localhost:5432   (the same port on the remote)
//	8080:3000            L:8080:localhost:3000
//	db.internal:5432     L:5432:db.internal:5432
//	8080:db.internal:5432
func Expand(s string) string {
	if spec, ok := Names[strings.ToLower(s)]; ok {
		return spec
	}
	if k, _, ok := strings.Cut(s, ":"); ok && len(k) == 1 && strings.ContainsAny(strings.ToUpper(k), "LRDHK") {
		return s // already has a kind
	}
	toks, err := split(s)
	if err != nil {
		return s
	}
	target := func(host, port string) string {
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		return host + ":" + port
	}
	switch {
	case len(toks) == 1 && isPort(toks[0]):
		return "L:" + toks[0] + ":localhost:" + toks[0]
	case len(toks) == 2 && isPort(toks[0]) && isPort(toks[1]):
		return "L:" + toks[0] + ":localhost:" + toks[1]
	case len(toks) == 2 && isPort(toks[1]):
		return "L:" + toks[1] + ":" + target(toks[0], toks[1])
	case len(toks) == 3 && isPort(toks[0]) && isPort(toks[2]):
		return "L:" + toks[0] + ":" + target(toks[1], toks[2])
	}
	return s
}

// ParseInput parses what someone typed: an optional "label=", then a spec,
// a name or a shorthand.
func ParseInput(s string) (label string, spec Spec, err error) {
	label, rest, err := SplitLabel(s)
	if err != nil {
		return "", Spec{}, err
	}
	spec, err = Parse(Expand(rest))
	return label, spec, err
}

// Describe says what spec does in plain words, for a forward over host's
// connection: "localhost:5432 here → db.internal:5432, reached from devbox".
func Describe(s Spec, host string) string {
	if host == "" {
		host = "the remote"
	}
	here := func(e Endpoint) string { return listenText(e) + " here" }
	there := func(e Endpoint) string {
		if e.IsSocket() {
			return e.Socket + " on " + host
		}
		return listenText(e) + " on " + host
	}
	switch {
	case s.Kind == Dynamic:
		return "SOCKS proxy at " + here(s.Listen) + ", connecting out from " + host
	case s.Kind == HTTP:
		return "HTTP and SOCKS proxy at " + here(s.Listen) + ", connecting out from " + host
	case s.IsReverseSOCKS():
		return "SOCKS proxy at " + there(s.Listen) + ", connecting out from this machine"
	case s.Kind == Kube:
		k := s.KubeRef()
		where := "namespace " + k.Namespace
		if k.Context != "" {
			where += ", context " + k.Context
		}
		return fmt.Sprintf("%s → %s port %d (%s), via kubectl on %s", here(s.Listen), k.Object(), s.Target.Port, where, host)
	case s.Kind == Local:
		return here(s.Listen) + " → " + targetText(s.Target, host)
	default: // Remote
		return there(s.Listen) + " → " + targetText(s.Target, "")
	}
}

// listenText is a listening endpoint for people: "localhost:5432",
// "all interfaces, port 5432", "a free port".
func listenText(e Endpoint) string {
	switch {
	case e.IsSocket():
		return e.Socket
	case e.Port == 0:
		return "a free port"
	case e.Host == "":
		return "localhost:" + strconv.Itoa(e.Port)
	case e.Host == "*" || e.Host == "0.0.0.0" || e.Host == "::":
		return "port " + strconv.Itoa(e.Port) + " on all interfaces"
	}
	return net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

// targetText is where a forward connects to. For a local forward (reached
// from host) "localhost" means host itself; for a remote one it means this
// machine.
func targetText(e Endpoint, host string) string {
	side := "here"
	if host != "" {
		side = "on " + host
	}
	switch {
	case e.IsSocket():
		return e.Socket + " " + side
	case e.Host == "localhost" || e.Host == "127.0.0.1" || e.Host == "::1":
		return "port " + strconv.Itoa(e.Port) + " " + side
	case host != "":
		return net.JoinHostPort(e.Host, strconv.Itoa(e.Port)) + ", reached from " + host
	}
	return net.JoinHostPort(e.Host, strconv.Itoa(e.Port)) + ", reached from this machine"
}
