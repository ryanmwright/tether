// Package api defines the daemon's RPC methods and their payloads. It is
// shared by the daemon and every client (CLI, TUI, tray).
package api

import "time"

const (
	MethodStatus   = "daemon.status"
	MethodShutdown = "daemon.shutdown"
	MethodReload   = "config.reload"
)

// Application error codes (outside the range reserved by JSON-RPC).
const (
	CodeInvalidConfig = 1000
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
	Version     string          `json:"version"`
	PID         int             `json:"pid"`
	StartedAt   time.Time       `json:"started_at"`
	ConfigPath  string          `json:"config_path"`
	ConfigError string          `json:"config_error,omitempty"`
	Hosts       []HostStatus    `json:"hosts"`
	Profiles    []ProfileStatus `json:"profiles"`
}

type HostStatus struct {
	Name        string `json:"name"`
	SSH         string `json:"ssh"`
	Autoconnect bool   `json:"autoconnect"`
	State       State  `json:"state"`
	Error       string `json:"error,omitempty"`
}

type ProfileStatus struct {
	Name        string `json:"name"`
	Host        string `json:"host"`
	Autoconnect bool   `json:"autoconnect"`
	State       State  `json:"state"`
	Error       string `json:"error,omitempty"`
}

type ReloadResult struct {
	Hosts    int `json:"hosts"`
	Profiles int `json:"profiles"`
}
