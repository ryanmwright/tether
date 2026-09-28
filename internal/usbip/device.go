// Package usbip shares USB devices on this machine with remotes over USB/IP.
//
// Binding a device for export and handing its connection to the kernel need
// root, so that part runs in a small privileged helper (RunHelper). The
// daemon lists devices itself, asks the helper to export them over its
// control socket, and runs `usbip attach` on the remote through a reverse
// forward to the helper's USB/IP port.
package usbip

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// SysfsRoot is where sysfs is mounted; tests point it elsewhere.
var SysfsRoot = "/sys"

// Stub driver states, from /sys/bus/usb/devices/BUSID/usbip_status.
const (
	StatusAvailable = 1 // bound to usbip-host, not attached anywhere
	StatusUsed      = 2 // attached to a remote
	StatusError     = 3
)

const stubDriver = "usbip-host"

// Device is a USB device on this machine, as sysfs describes it.
type Device struct {
	BusID   string // "1-1.2"
	Vendor  string // "1050"
	Product string // "0407"
	Name    string // "Yubico YubiKey OTP+FIDO+CCID", from the device's strings
	Serial  string
	Driver  string // "usb" normally, "usbip-host" while exported
	Status  int    // usbip_status while exported, else 0

	// For the USB/IP wire protocol.
	Path                                   string
	BusNum, DevNum, Speed                  uint32
	BcdDevice                              uint16
	Class, SubClass, Protocol              uint8
	ConfigValue, NumConfigs, NumInterfaces uint8
	Interfaces                             [][3]uint8 // class, subclass, protocol
}

// ID is "vendor:product", e.g. "1050:0407".
func (d Device) ID() string { return d.Vendor + ":" + d.Product }

// Title names the device for people: "YubiKey (1050:0407)".
func (d Device) Title() string {
	if d.Name == "" {
		return d.ID()
	}
	return d.Name + " (" + d.ID() + ")"
}

var busIDPattern = regexp.MustCompile(`^[0-9]+-[0-9]+(\.[0-9]+)*$`)

func devicesDir() string { return filepath.Join(SysfsRoot, "bus/usb/devices") }

// List returns the USB devices plugged in here that can be shared: every
// device except hubs, sorted by bus ID.
func List() ([]Device, error) {
	entries, err := os.ReadDir(devicesDir())
	if err != nil {
		return nil, err
	}
	var devs []Device
	for _, e := range entries {
		if !busIDPattern.MatchString(e.Name()) {
			continue // root hubs (usb1) and interfaces (1-1:1.0)
		}
		d, err := Read(e.Name())
		if err != nil || d.Class == 0x09 {
			continue // unplugged meanwhile, or a hub
		}
		devs = append(devs, d)
	}
	slices.SortFunc(devs, func(a, b Device) int { return compareBusID(a.BusID, b.BusID) })
	return devs, nil
}

// compareBusID orders "1-2" before "1-10", numerically by part.
func compareBusID(a, b string) int {
	split := func(s string) []int {
		var n []int
		for f := range strings.FieldsFuncSeq(s, func(r rune) bool { return r == '-' || r == '.' }) {
			v, _ := strconv.Atoi(f)
			n = append(n, v)
		}
		return n
	}
	return slices.Compare(split(a), split(b))
}

// Read describes the device at busid.
func Read(busid string) (Device, error) {
	if !busIDPattern.MatchString(busid) {
		return Device{}, fmt.Errorf("invalid bus ID %q", busid)
	}
	dir := filepath.Join(devicesDir(), busid)
	path, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return Device{}, fmt.Errorf("no USB device at %s", busid)
	}
	a := attrs{dir: dir}
	d := Device{
		BusID:         busid,
		Path:          strings.TrimPrefix(path, SysfsRoot),
		Vendor:        a.str("idVendor"),
		Product:       a.str("idProduct"),
		Serial:        a.str("serial"),
		BusNum:        uint32(a.dec("busnum")),
		DevNum:        uint32(a.dec("devnum")),
		Speed:         speedCode(a.str("speed")),
		BcdDevice:     uint16(a.hex("bcdDevice")),
		Class:         uint8(a.hex("bDeviceClass")),
		SubClass:      uint8(a.hex("bDeviceSubClass")),
		Protocol:      uint8(a.hex("bDeviceProtocol")),
		ConfigValue:   uint8(a.dec("bConfigurationValue")),
		NumConfigs:    uint8(a.dec("bNumConfigurations")),
		NumInterfaces: uint8(a.dec("bNumInterfaces")),
	}
	if a.err != nil {
		return Device{}, a.err
	}
	d.Name = strings.TrimSpace(a.str("manufacturer") + " " + a.str("product"))
	if drv, err := os.Readlink(filepath.Join(dir, "driver")); err == nil {
		d.Driver = filepath.Base(drv)
	}
	if d.Driver == stubDriver {
		d.Status = a.dec("usbip_status")
	}
	for i := range int(d.NumInterfaces) {
		ia := attrs{dir: filepath.Join(dir, fmt.Sprintf("%s:%d.%d", busid, d.ConfigValue, i))}
		d.Interfaces = append(d.Interfaces, [3]uint8{
			uint8(ia.hex("bInterfaceClass")), uint8(ia.hex("bInterfaceSubClass")), uint8(ia.hex("bInterfaceProtocol")),
		})
	}
	return d, nil
}

// attrs reads sysfs attributes, remembering the first required one that's
// missing. Optional ones (strings) are empty when missing.
type attrs struct {
	dir string
	err error
}

func (a *attrs) str(name string) string {
	b, _ := os.ReadFile(filepath.Join(a.dir, name))
	return strings.TrimSpace(string(b))
}

func (a *attrs) num(name string, base int) int {
	b, err := os.ReadFile(filepath.Join(a.dir, name))
	if err != nil {
		if a.err == nil && name != "usbip_status" {
			a.err = err
		}
		return 0
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0 // bConfigurationValue of an unconfigured device
	}
	v, _ := strconv.ParseInt(s, base, 32)
	return int(v)
}

func (a *attrs) dec(name string) int { return a.num(name, 10) }
func (a *attrs) hex(name string) int { return a.num(name, 16) }

// speedCode maps sysfs's speed (Mbit/s) to the kernel's enum usb_device_speed,
// which USB/IP sends. SuperSpeed+ is sent as SuperSpeed, which vhci supports
// everywhere.
func speedCode(mbps string) uint32 {
	switch mbps {
	case "1.5":
		return 1
	case "12":
		return 2
	case "480":
		return 3
	case "53.3-480":
		return 4
	case "5000", "10000", "20000":
		return 5
	}
	return 0
}

// Spec names a device to share: a bus ID ("1-2.3", a port) or a
// "vendor:product" ID ("1050:0407", whichever port it's in).
type Spec string

var idPattern = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{4}$`)

// ParseSpec checks s and returns it in canonical form.
func ParseSpec(s string) (Spec, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if busIDPattern.MatchString(s) || idPattern.MatchString(s) {
		return Spec(s), nil
	}
	return "", fmt.Errorf("invalid USB device %q: use a bus ID (1-2.3) or vendor:product (1050:0407); see `tether usb list`", s)
}

func (s Spec) Matches(d Device) bool {
	return string(s) == d.BusID || string(s) == d.ID()
}

// Find picks the device s names. A vendor:product that matches several
// devices is ambiguous.
func (s Spec) Find(devs []Device) (Device, error) {
	var found []Device
	for _, d := range devs {
		if s.Matches(d) {
			found = append(found, d)
		}
	}
	switch len(found) {
	case 0:
		return Device{}, fmt.Errorf("no USB device %s plugged in", s)
	case 1:
		return found[0], nil
	}
	ids := make([]string, len(found))
	for i, d := range found {
		ids[i] = d.BusID
	}
	return Device{}, fmt.Errorf("%s matches several devices (bus IDs %s); use a bus ID", s, strings.Join(ids, ", "))
}
