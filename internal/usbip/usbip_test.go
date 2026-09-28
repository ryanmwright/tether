package usbip

import (
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeSysfs builds a sysfs tree with a device at 1-1 (bound to usb), a hub
// at 1-2, a device behind it at 1-2.10 (exported and in use), and 1-2.3.
func fakeSysfs(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	old := SysfsRoot
	SysfsRoot = root
	t.Cleanup(func() { SysfsRoot = old })

	devices := filepath.Join(root, "bus/usb/devices")
	os.MkdirAll(devices, 0o755)
	for _, drv := range []string{"usb", "usbip-host"} {
		os.MkdirAll(filepath.Join(root, "bus/usb/drivers", drv), 0o755)
	}
	add := func(busid string, attrs map[string]string, driver string) {
		real := filepath.Join(root, "devices/pci0000:00/usb1", busid)
		os.MkdirAll(real, 0o755)
		base := map[string]string{
			"idVendor": "1050", "idProduct": "0407", "busnum": "1", "devnum": "5", "speed": "12",
			"bcdDevice": "0543", "bDeviceClass": "00", "bDeviceSubClass": "00", "bDeviceProtocol": "00",
			"bConfigurationValue": "1", "bNumConfigurations": "1", "bNumInterfaces": " 1",
		}
		for k, v := range attrs {
			base[k] = v
		}
		for k, v := range base {
			os.WriteFile(filepath.Join(real, k), []byte(v+"\n"), 0o644)
		}
		iface := filepath.Join(real, busid+":1.0")
		os.MkdirAll(iface, 0o755)
		for k, v := range map[string]string{"bInterfaceClass": "03", "bInterfaceSubClass": "01", "bInterfaceProtocol": "02"} {
			os.WriteFile(filepath.Join(iface, k), []byte(v+"\n"), 0o644)
		}
		if driver != "" {
			os.Symlink(filepath.Join(root, "bus/usb/drivers", driver), filepath.Join(real, "driver"))
		}
		os.Symlink(real, filepath.Join(devices, busid))
	}
	add("1-1", map[string]string{"manufacturer": "Yubico", "product": "YubiKey OTP+FIDO+CCID"}, "usb")
	add("1-2", map[string]string{"bDeviceClass": "09", "idVendor": "1d6b"}, "usb")
	add("1-2.10", map[string]string{"idVendor": "0627", "idProduct": "0001", "usbip_status": "2", "speed": "480"}, "usbip-host")
	add("1-2.3", map[string]string{"idVendor": "0627", "idProduct": "0001", "bConfigurationValue": ""}, "usb")
	// Root hubs and interfaces aren't devices to share.
	os.Symlink(filepath.Join(root, "devices/pci0000:00/usb1"), filepath.Join(devices, "usb1"))
	os.Symlink(filepath.Join(root, "devices/pci0000:00/usb1/1-1/1-1:1.0"), filepath.Join(devices, "1-1:1.0"))
}

func TestList(t *testing.T) {
	fakeSysfs(t)
	devs, err := List()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range devs {
		ids = append(ids, d.BusID)
	}
	if want := []string{"1-1", "1-2.3", "1-2.10"}; !slices.Equal(ids, want) {
		t.Fatalf("bus IDs = %v, want %v (hubs skipped, numeric order)", ids, want)
	}

	yk := devs[0]
	if yk.Title() != "Yubico YubiKey OTP+FIDO+CCID (1050:0407)" || yk.Driver != "usb" || yk.Status != 0 {
		t.Errorf("yubikey = %+v", yk)
	}
	if yk.Speed != 2 || yk.BcdDevice != 0x0543 || yk.NumInterfaces != 1 || yk.Path != "/devices/pci0000:00/usb1/1-1" {
		t.Errorf("yubikey wire fields = %+v", yk)
	}
	if !slices.Equal(yk.Interfaces, [][3]uint8{{3, 1, 2}}) {
		t.Errorf("interfaces = %v", yk.Interfaces)
	}
	if d := devs[2]; d.Driver != stubDriver || d.Status != StatusUsed || d.Speed != 3 {
		t.Errorf("exported device = %+v", d)
	}
	if d := devs[1]; d.ConfigValue != 0 || d.Title() != "0627:0001" {
		t.Errorf("unconfigured device = %+v", d)
	}
}

func TestSpec(t *testing.T) {
	fakeSysfs(t)
	devs, _ := List()
	for _, tt := range []struct {
		in, want, err string
	}{
		{"1-1", "1-1", ""},
		{" 1050:0407 ", "1050:0407", ""},
		{"1050:0407", "1-1", ""},
		{"1-2.10", "1-2.10", ""},
		{"0627:0001", "", "matches several devices (bus IDs 1-2.3, 1-2.10)"},
		{"dead:beef", "", "no USB device dead:beef plugged in"},
		{"usb1", "", "invalid USB device"},
		{"1-1:1.0", "", "invalid USB device"},
		{"1050:407", "", "invalid USB device"},
	} {
		spec, err := ParseSpec(tt.in)
		if err == nil {
			var d Device
			d, err = spec.Find(devs)
			if err == nil && tt.want != d.BusID && tt.want != string(spec) {
				t.Errorf("%q: found %s, want %s", tt.in, d.BusID, tt.want)
			}
		}
		if tt.err == "" && err != nil || tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)) {
			t.Errorf("%q: err = %v, want %q", tt.in, err, tt.err)
		}
	}
	if s, _ := ParseSpec("1050:0407"); s != "1050:0407" {
		t.Errorf("canonical form = %q", s)
	}
	if s, _ := ParseSpec("1050:ABCD"); s != "1050:abcd" {
		t.Errorf("hex is lowercased: %q", s)
	}
}

func TestEncode(t *testing.T) {
	fakeSysfs(t)
	d, err := Read("1-1")
	if err != nil {
		t.Fatal(err)
	}
	b := encodeDevice(d)
	if len(b) != deviceSize || deviceSize != 312 {
		t.Fatalf("device record is %d bytes, want 312", len(b))
	}
	if parseCString(b[:pathSize]) != d.Path || parseCString(b[pathSize:pathSize+busIDSize]) != "1-1" {
		t.Errorf("path/busid = %q %q", parseCString(b[:pathSize]), parseCString(b[pathSize:pathSize+busIDSize]))
	}
	rest := b[pathSize+busIDSize:]
	if busnum, devnum, speed := binary.BigEndian.Uint32(rest), binary.BigEndian.Uint32(rest[4:]), binary.BigEndian.Uint32(rest[8:]); busnum != 1 || devnum != 5 || speed != 2 {
		t.Errorf("busnum/devnum/speed = %d/%d/%d", busnum, devnum, speed)
	}
	if v, p := binary.BigEndian.Uint16(rest[12:]), binary.BigEndian.Uint16(rest[14:]); v != 0x1050 || p != 0x0407 {
		t.Errorf("vendor/product = %04x:%04x", v, p)
	}
	if rest[23] != 1 {
		t.Errorf("bNumInterfaces = %d", rest[23])
	}

	list := encodeDevlist([]Device{d})
	if len(list) != 8+4+312+4 {
		t.Fatalf("devlist is %d bytes", len(list))
	}
	if code, n := binary.BigEndian.Uint16(list[2:]), binary.BigEndian.Uint32(list[8:]); code != opRepDevlist || n != 1 {
		t.Errorf("devlist header: code %#x, %d devices", code, n)
	}
	if iface := list[len(list)-4:]; iface[0] != 3 || iface[1] != 1 || iface[2] != 2 {
		t.Errorf("interface = %v", iface)
	}
}

func TestFindPort(t *testing.T) {
	out := `Imported USB devices
====================
Port 00: <Port in Use> at High Speed(480Mbps)
       Yubico.com : Yubikey 4/5 OTP+U2F+CCID (1050:0407)
       3-1 -> usbip://127.0.0.1:41234/1-2
           -> remote bus/dev 001/005
Port 09: <Port in Use> at Full Speed(12Mbps)
       unknown vendor : unknown product (0627:0001)
       3-2 -> usbip://127.0.0.1:41234/1-1
           -> remote bus/dev 001/002
`
	if p, ok := FindPort(out, 41234, "1-1"); !ok || p != 9 {
		t.Errorf("1-1 = %d %v, want port 9", p, ok)
	}
	if p, ok := FindPort(out, 41234, "1-2"); !ok || p != 0 {
		t.Errorf("1-2 = %d %v, want port 0", p, ok)
	}
	// Another tether connection's device (different forward port), or a
	// device that isn't there.
	if _, ok := FindPort(out, 5555, "1-1"); ok {
		t.Error("matched another port")
	}
	if _, ok := FindPort(out, 41234, "1-1.2"); ok {
		t.Error("matched a prefix of the bus ID")
	}
}

func TestFindSocketUID(t *testing.T) {
	table := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0CAC 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 0100007F:0CAC 0100007F:A0B2 01 00000000:00000000 00:00000000 00000000     0        0 2 1 0000000000000000 20 4 30 10 -1
   2: 0100007F:A0B2 0100007F:0CAC 01 00000000:00000000 00:00000000 00000000  1000        0 3 1 0000000000000000 20 4 30 10 -1
`
	peer := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0xA0B2}
	us := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3244}
	if uid, err := findSocketUID(strings.NewReader(table), peer, us); err != nil || uid != 1000 {
		t.Errorf("uid = %d, %v; want 1000", uid, err)
	}
	other := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
	if _, err := findSocketUID(strings.NewReader(table), other, us); err == nil {
		t.Error("found a socket that isn't there")
	}
}

func TestRemoteCommandQuoting(t *testing.T) {
	cmd := RemoteCommand("echo \"$1\"", "it's")
	if !strings.HasSuffix(cmd, ` tether-usb 'it'"'"'s'`) {
		t.Errorf("command = %s", cmd)
	}
}
