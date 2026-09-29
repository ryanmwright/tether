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
	MethodUSBAttach     = "usb.attach"
	MethodUSBDetach     = "usb.detach"
	MethodDoctor        = "host.doctor"
	MethodLogs          = "daemon.logs"
	MethodKubeList      = "kube.list"
	MethodKubeGC        = "kube.gc"
	MethodKubeTargets   = "kube.targets"

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
const ProtocolVersion = 6

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
	// USB lists the devices plugged in here that can be shared.
	USB []USBDevice `json:"usb"`
	// USBUnavailable says why devices can't be shared right now, e.g. the
	// USB/IP helper isn't running.
	USBUnavailable string `json:"usb_unavailable,omitempty"`
}

// USBDevice is a USB device on this machine.
type USBDevice struct {
	BusID string `json:"busid"` // e.g. "1-2.3"
	ID    string `json:"id"`    // vendor:product, e.g. "1050:0407"
	Name  string `json:"name,omitempty"`
	// Host is where the device is attached (or being attached), if anywhere.
	Host string `json:"host,omitempty"`
}

// Title names the device for people: "YubiKey (1050:0407)".
func (d USBDevice) Title() string {
	if d.Name == "" {
		return d.ID
	}
	return d.Name + " (" + d.ID + ")"
}

type HostStatus struct {
	Name        string `json:"name"`
	SSH         string `json:"ssh"`
	Autoconnect bool   `json:"autoconnect"`
	AdHoc       bool   `json:"adhoc,omitempty"` // added with host.add, not in the config
	Local       bool   `json:"local,omitempty"` // this machine, no SSH: only PVC mounts
	// RecentPVCs are claims mounted from this host lately, newest first, to
	// mount again quickly.
	RecentPVCs []RecentPVC `json:"recent_pvcs,omitempty"`
	// RecentForwards are ad-hoc forwards added lately, newest first.
	RecentForwards []RecentForward `json:"recent_forwards,omitempty"`
	State          State           `json:"state"`
	Error          string          `json:"error,omitempty"`
	RetryAt        *time.Time      `json:"retry_at,omitempty"` // next reconnect attempt
	Forwards       []ForwardStatus `json:"forwards"`
	Mounts         []MountStatus   `json:"mounts"`
	USB            []USBStatus     `json:"usb"`
}

// USBStatus is a USB device shared with a host.
type USBStatus struct {
	Device   string   `json:"device"`          // as asked for: a bus ID or vendor:product
	BusID    string   `json:"busid,omitempty"` // the device it matched, once found
	Name     string   `json:"name,omitempty"`
	Profiles []string `json:"profiles,omitempty"`
	AdHoc    bool     `json:"adhoc,omitempty"`
	State    State    `json:"state"`
	Error    string   `json:"error,omitempty"`
}

type MountStatus struct {
	Key       string   `json:"key"`       // e.g. "remote:~/src -> /home/me/mnt/src"
	Direction string   `json:"direction"` // remote-to-local, local-to-remote or pvc-to-local
	Remote    string   `json:"remote"`    // for a PVC, "pvc:context/namespace/claim"
	Local     string   `json:"local"`
	Profiles  []string `json:"profiles,omitempty"`
	AdHoc     bool     `json:"adhoc,omitempty"`
	State     State    `json:"state"`
	Error     string   `json:"error,omitempty"`
	// Kube describes a PVC mount, and its helper pod once running.
	Kube *KubeMountStatus `json:"kube,omitempty"`
}

// KubeMount names a Kubernetes claim to mount.
type KubeMount struct {
	Context   string `json:"context,omitempty"` // empty for kubectl's current context
	Namespace string `json:"namespace"`
	PVC       string `json:"pvc"`
	SubPath   string `json:"sub_path,omitempty"`
	ReadOnly  bool   `json:"read_only,omitempty"`
}

// RecentPVC is a claim mounted lately, and where.
type RecentPVC struct {
	KubeMount
	Local string `json:"local"`
}

type KubeMountStatus struct {
	KubeMount
	Pod  string `json:"pod,omitempty"`  // the helper pod
	Node string `json:"node,omitempty"` // where it runs
}

type ForwardStatus struct {
	Spec  string `json:"spec"`            // canonical form
	Label string `json:"label,omitempty"` // a name given to it, e.g. "postgres"
	// Description says what it does in plain words.
	Description string `json:"description,omitempty"`
	// Address is where to connect to it here, e.g. "localhost:5432" or a
	// socket path; empty if it listens on the remote.
	Address string `json:"address,omitempty"`
	// Target is the result of checking what it forwards to: "ok",
	// "unreachable", or empty if not checked (proxies, or checks are off).
	Target      string   `json:"target,omitempty"`
	TargetError string   `json:"target_error,omitempty"`
	Profiles    []string `json:"profiles,omitempty"` // active profiles that include it
	AdHoc       bool     `json:"adhoc,omitempty"`    // added with forward.add
	State       State    `json:"state"`
	Error       string   `json:"error,omitempty"`
	// AllocatedPort is the port chosen for a forward on port 0: by the
	// server for a remote one, by tether for a local one.
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

// ForwardParams names a forward for forward.add and forward.remove. Spec
// may be a spec, a shorthand (5432, db:5432), a name (socks, rsocks, http,
// gpg-agent, gpg-ssh), and for forward.add may start with "label=".
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
// absolute or start with ~/ (the daemon's home). For direction pvc-to-local,
// Kube names the claim instead of Remote, and an empty Local means the
// default mount point.
type MountParams struct {
	Host      string     `json:"host"`
	Direction string     `json:"direction"`
	Remote    string     `json:"remote,omitempty"`
	Local     string     `json:"local"`
	Options   []string   `json:"options,omitempty"`
	Kube      *KubeMount `json:"kube,omitempty"`
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

// USBParams names a device for usb.attach and usb.detach: a bus ID ("1-2.3")
// or vendor:product ("1050:0407").
type USBParams struct {
	Host   string `json:"host"`
	Device string `json:"device"`
}

type USBResult struct {
	Host       string `json:"host"`
	Device     string `json:"device"` // canonical form
	Generation uint64 `json:"generation"`
}

// KubeListParams asks for the claims kubectl on Host can see. An empty
// Context means kubectl's current one; an empty Namespace means all of them.
type KubeListParams struct {
	Host      string `json:"host"`
	Context   string `json:"context,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

type KubeListResult struct {
	Host     string   `json:"host"`
	Context  string   `json:"context"` // the context listed
	Current  string   `json:"current"` // kubectl's current context
	Contexts []string `json:"contexts"`
	PVCs     []PVC    `json:"pvcs"`
	// MountRoot is where claims are mounted by default, as
	// MountRoot/<context>/<namespace>/<claim>.
	MountRoot string `json:"mount_root"`
}

// PVC is a claim as listed for picking one to mount.
type PVC struct {
	Namespace    string        `json:"namespace"`
	Name         string        `json:"name"`
	Phase        string        `json:"phase"` // Bound, Pending, Lost
	Capacity     string        `json:"capacity,omitempty"`
	Request      string        `json:"request,omitempty"`
	StorageClass string        `json:"storage_class,omitempty"`
	AccessModes  []string      `json:"access_modes,omitempty"` // abbreviated: RWO, ROX, RWX, RWOP
	BindingMode  string        `json:"binding_mode,omitempty"` // Immediate, WaitForFirstConsumer; empty if unknown
	Deleting     bool          `json:"deleting,omitempty"`
	UsedBy       []PVCConsumer `json:"used_by,omitempty"`
	// Mountable says whether a helper pod can mount it now; Note explains
	// why not, or what mounting will do (pin to a node, provision a volume).
	Mountable bool   `json:"mountable"`
	Note      string `json:"note,omitempty"`
	// Node is where the helper pod must run: the node of a pod already using
	// a ReadWriteOnce volume.
	Node string `json:"node,omitempty"`
}

// Ref is "namespace/claim".
func (p PVC) Ref() string { return p.Namespace + "/" + p.Name }

// Size is the claim's capacity, or its request while it has none.
func (p PVC) Size() string {
	if p.Capacity != "" {
		return p.Capacity
	}
	return p.Request
}

// PVCConsumer is a pod that uses a claim.
type PVCConsumer struct {
	Pod    string `json:"pod"`
	Node   string `json:"node,omitempty"`
	Phase  string `json:"phase,omitempty"`
	Tether bool   `json:"tether,omitempty"` // a tether helper pod
}

// KubeGCParams asks to delete leftover helper pods in a context (empty for
// kubectl's current one).
type KubeGCParams struct {
	Host    string `json:"host"`
	Context string `json:"context,omitempty"`
}

type KubeGCResult struct {
	Deleted int `json:"deleted"`
}

// KubeTargetsParams asks for the services and pods kubectl on Host can
// forward to. Empty Context: kubectl's current one; empty Namespace: all.
type KubeTargetsParams struct {
	Host      string `json:"host"`
	Context   string `json:"context,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

type KubeTargetsResult struct {
	Host     string       `json:"host"`
	Context  string       `json:"context"`
	Current  string       `json:"current"`
	Contexts []string     `json:"contexts"`
	Targets  []KubeTarget `json:"targets"`
}

// KubeTarget is a service or pod to forward to.
type KubeTarget struct {
	Namespace string     `json:"namespace"`
	Kind      string     `json:"kind"` // svc or pod
	Name      string     `json:"name"`
	Ports     []KubePort `json:"ports,omitempty"`
	Owner     string     `json:"owner,omitempty"` // for pods: e.g. ReplicaSet/web-5d9f
}

// Ref is "namespace/kind/name".
func (t KubeTarget) Ref() string { return t.Namespace + "/" + t.Kind + "/" + t.Name }

type KubePort struct {
	Name string `json:"name,omitempty"`
	Port int    `json:"port"`
}

// RecentForward is a forward added lately.
type RecentForward struct {
	Spec  string `json:"spec"`
	Label string `json:"label,omitempty"`
}
