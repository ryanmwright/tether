// Package api defines the daemon's RPC methods and their payloads. It is
// shared by the daemon and every client (CLI, TUI, tray).
package api

import (
	"strconv"
	"strings"
	"time"
)

const (
	MethodStatus        = "daemon.status"
	MethodShutdown      = "daemon.shutdown"
	MethodReload        = "config.reload"
	MethodUp            = "session.up"
	MethodDown          = "session.down"
	MethodForwardAdd    = "forward.add"
	MethodForwardRemove = "forward.remove"
	MethodMountAdd      = "mount.add"
	MethodMountRemove   = "mount.remove"
	MethodHostAdd       = "host.add"
	MethodHostRemove    = "host.remove"
	MethodDoctor        = "host.doctor"
	MethodLogs          = "daemon.logs"

	// MethodSubscribe makes the daemon push EventStatus notifications, each
	// carrying a full Status, now and whenever anything changes; and, if
	// SubscribeParams.Logs is set, an EventLog for every new log entry.
	MethodSubscribe = "events.subscribe"
	EventStatus     = "event.status"
	EventLog        = "event.log"
)

// ProtocolVersion identifies the API a daemon speaks. Bump it whenever a
// method or field that clients rely on is added or changed, so a client can
// tell it's talking to an older daemon left running across an upgrade.
// Daemons from before it existed report 0.
const ProtocolVersion = 3

// Application error codes (outside the range reserved by JSON-RPC).
const (
	CodeInvalidConfig = 1000
	CodeNotFound      = 1001
	CodeAmbiguous     = 1002
)

type State string

const (
	StatePending  State = "pending"
	StateUp       State = "up"
	StateDegraded State = "degraded"
	StateDown     State = "down"
	StateError    State = "error"
)

type Status struct {
	// Generation increases with every state change. A snapshot reflects a
	// request once its Generation reaches the one the request returned.
	Generation  uint64          `json:"generation"`
	Protocol    int             `json:"protocol"` // api.ProtocolVersion
	Version     string          `json:"version"`
	PID         int             `json:"pid"`
	StartedAt   time.Time       `json:"started_at"`
	ConfigPath  string          `json:"config_path"`
	ConfigError string          `json:"config_error,omitempty"`
	Hosts       []HostStatus    `json:"hosts"`
	Profiles    []ProfileStatus `json:"profiles"`
}

type HostStatus struct {
	Name        string          `json:"name"`
	SSH         string          `json:"ssh"`
	Autoconnect bool            `json:"autoconnect"`
	AdHoc       bool            `json:"adhoc,omitempty"` // added with host.add, not in the config
	State       State           `json:"state"`
	Error       string          `json:"error,omitempty"`
	RetryAt     *time.Time      `json:"retry_at,omitempty"` // next reconnect attempt
	Forwards    []ForwardStatus `json:"forwards"`
	Mounts      []MountStatus   `json:"mounts"`
}

type MountStatus struct {
	Key       string   `json:"key"`       // e.g. "remote:~/src -> /home/me/mnt/src"
	Direction string   `json:"direction"` // remote-to-local or local-to-remote
	Remote    string   `json:"remote"`
	Local     string   `json:"local"`
	Profiles  []string `json:"profiles,omitempty"`
	AdHoc     bool     `json:"adhoc,omitempty"`
	State     State    `json:"state"`
	Error     string   `json:"error,omitempty"`
}

type ForwardStatus struct {
	Spec     string   `json:"spec"`               // canonical form
	Profiles []string `json:"profiles,omitempty"` // active profiles that include it
	AdHoc    bool     `json:"adhoc,omitempty"`    // added with forward.add
	State    State    `json:"state"`
	Error    string   `json:"error,omitempty"`
	// AllocatedPort is the server-chosen port for a remote forward on port 0.
	AllocatedPort int `json:"allocated_port,omitempty"`
	// Resolved is the concrete forward behind a named one (gpg-agent,
	// gpg-ssh), known once it is up.
	Resolved string `json:"resolved,omitempty"`
}

type ProfileStatus struct {
	Name        string `json:"name"`
	Host        string `json:"host"`
	Autoconnect bool   `json:"autoconnect"`
	Active      bool   `json:"active"`
	State       State  `json:"state"`
	Error       string `json:"error,omitempty"`
}

type TargetKind string

const (
	TargetHost    TargetKind = "host"
	TargetProfile TargetKind = "profile"
)

// TargetParams names a host or profile for session.up and session.down.
// Kind may be empty when the name is unambiguous.
type TargetParams struct {
	Name string     `json:"name"`
	Kind TargetKind `json:"kind,omitempty"`
}

type TargetResult struct {
	Name       string     `json:"name"`
	Kind       TargetKind `json:"kind"`
	Host       string     `json:"host"`
	Generation uint64     `json:"generation"`
}

type ForwardParams struct {
	Host string `json:"host"`
	Spec string `json:"spec"`
}

type ForwardResult struct {
	Host       string `json:"host"`
	Spec       string `json:"spec"` // canonical form
	Generation uint64 `json:"generation"`
}

type ReloadResult struct {
	Hosts    int `json:"hosts"`
	Profiles int `json:"profiles"`
}

type DoctorParams struct {
	Host string `json:"host"`
}

type CheckStatus string

const (
	CheckOK   CheckStatus = "ok"
	CheckWarn CheckStatus = "warn"
	CheckFail CheckStatus = "fail"
	CheckSkip CheckStatus = "skip"
)

type Check struct {
	Section string      `json:"section"` // local, connection, remote
	Name    string      `json:"name"`
	Status  CheckStatus `json:"status"`
	Detail  string      `json:"detail,omitempty"`
	Fix     string      `json:"fix,omitempty"` // what to do about a warning or failure
}

type DoctorResult struct {
	Host   string  `json:"host"`
	Checks []Check `json:"checks"`
}

type SubscribeParams struct {
	Logs bool `json:"logs,omitempty"`
}

type LogsParams struct {
	Limit int `json:"limit,omitempty"` // 0 means everything kept
}

type LogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	Attrs   []LogAttr `json:"attrs,omitempty"`
}

type LogAttr struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Line formats e on one line: "15:04:05 INFO message key=value ...".
func (e LogEntry) Line() string {
	var b strings.Builder
	b.WriteString(e.Time.Local().Format("15:04:05"))
	b.WriteByte(' ')
	b.WriteString(e.Level)
	b.WriteByte(' ')
	b.WriteString(e.Message)
	for _, a := range e.Attrs {
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		if a.Value == "" || strings.ContainsAny(a.Value, " \t\"=") {
			b.WriteString(strconv.Quote(a.Value))
		} else {
			b.WriteString(a.Value)
		}
	}
	return b.String()
}

// MountParams names a mount for mount.add and mount.remove. Local must be
// absolute or start with ~/ (the daemon's home).
type MountParams struct {
	Host      string   `json:"host"`
	Direction string   `json:"direction"`
	Remote    string   `json:"remote"`
	Local     string   `json:"local"`
	Options   []string `json:"options,omitempty"`
}

type MountResult struct {
	Host       string `json:"host"`
	Key        string `json:"key"`
	Generation uint64 `json:"generation"`
}

// HostParams names an ad-hoc host for host.add (which also connects it) and
// host.remove. SSH defaults to Name.
type HostParams struct {
	Name string `json:"name"`
	SSH  string `json:"ssh,omitempty"`
}
