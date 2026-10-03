package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/ryanmwright/tether/internal/api"
	"github.com/ryanmwright/tether/internal/forward"
	"github.com/ryanmwright/tether/internal/openssh"
	"github.com/ryanmwright/tether/internal/usbip"
)

const (
	usbPollInterval = 2 * time.Second
	usbAttachWait   = 5 * time.Second
	// usbReleaseWait is how long moving a device waits for the host it's
	// leaving to give it back: a remote detach, then the helper's.
	usbReleaseWait = 15 * time.Second
)

// usbBackend is this machine's side of USB sharing: the devices plugged in,
// and the privileged helper that exports them.
type usbBackend interface {
	Devices() []usbip.Device
	Port(ctx context.Context) (int, error) // the helper's USB/IP port on 127.0.0.1
	Export(ctx context.Context, busid string) error
	Unexport(ctx context.Context, busid string) error
}

// localUSB polls sysfs for devices and talks to the helper.
type localUSB struct {
	helper *usbip.Client

	mu          sync.Mutex
	devs        []usbip.Device
	unavailable string // why sharing can't work right now
}

func (u *localUSB) Devices() []usbip.Device {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.devs
}

func (u *localUSB) Port(ctx context.Context) (int, error) { return u.helper.Port(ctx) }

func (u *localUSB) Export(ctx context.Context, busid string) error {
	return u.helper.Export(ctx, busid)
}

func (u *localUSB) Unexport(ctx context.Context, busid string) error {
	return u.helper.Unexport(ctx, busid)
}

// poll refreshes the device list and reports whether anything changed,
// including a device's export state: that is how sessions notice an
// attachment dropping.
func (u *localUSB) poll() bool {
	devs, err := usbip.List()
	unavailable := ""
	switch {
	case err != nil:
		unavailable = "can't list USB devices: " + err.Error()
	case !socketExists(u.helper.Socket):
		unavailable = usbip.ErrNoHelper.Error()
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if reflect.DeepEqual(devs, u.devs) && unavailable == u.unavailable {
		return false
	}
	u.devs, u.unavailable = devs, unavailable
	return true
}

func socketExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().Type() == os.ModeSocket
}

// watchUSB keeps the device list current and wakes sessions when it
// changes.
func (d *Daemon) watchUSB(ctx context.Context) {
	t := time.NewTicker(usbPollInterval)
	defer t.Stop()
	for {
		if d.usb.poll() {
			d.mu.Lock()
			for _, h := range d.sessions {
				poke(h.s.wake)
			}
			d.mu.Unlock()
			d.notify()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// usbStatus lists local devices for status, with the host each one is
// attached to. Callers hold d.mu.
func (d *Daemon) usbStatus(hosts []api.HostStatus) ([]api.USBDevice, string) {
	d.usb.mu.Lock()
	devs, unavailable := d.usb.devs, d.usb.unavailable
	d.usb.mu.Unlock()
	out := []api.USBDevice{}
	for _, dev := range devs {
		ud := api.USBDevice{BusID: dev.BusID, ID: dev.ID(), Name: dev.Name}
		for _, h := range hosts {
			for _, u := range h.USB {
				if u.BusID == dev.BusID {
					ud.Host = h.Name
				}
			}
		}
		out = append(out, ud)
	}
	return out, unavailable
}

type wantUSB struct {
	spec     usbip.Spec
	profiles []string
	adhoc    bool
}

type usbState struct {
	state api.State
	err   string
	busid string
	name  string
}

// usbRuntime is what the session's run loop knows about USB on the current
// connection.
type usbRuntime struct {
	fwd      forward.Spec // reverse forward to the helper
	port     int          // its port on the remote; 0 when not set up
	attached map[string]string
}

func newUSBRuntime() *usbRuntime { return &usbRuntime{attached: map[string]string{}} }

// syncUSB detaches devices no longer wanted, notices attachments that
// dropped, and attaches what's missing. It reports whether anything failed,
// so the caller can schedule a retry.
func (s *session) syncUSB(ctx context.Context, m *openssh.Master, w want, rt *usbRuntime) (failed bool) {
	for _, key := range slices.Sorted(maps.Keys(rt.attached)) {
		if _, ok := w.usb[key]; !ok {
			s.detachUSB(ctx, m, rt, key)
		}
	}
	if len(w.usb) == 0 {
		if rt.port != 0 {
			cctx, cancel := context.WithTimeout(ctx, controlTimeout)
			m.Cancel(cctx, rt.fwd)
			cancel()
			rt.port = 0
		}
		return false
	}

	devs := s.usb.Devices()
	for _, key := range slices.Sorted(maps.Keys(w.usb)) {
		if busid, ok := rt.attached[key]; ok {
			// Read afresh: the polled list may predate the attachment.
			d, err := usbip.Read(busid)
			if err == nil && d.Status == usbip.StatusUsed {
				continue
			}
			reason := "the remote dropped the device"
			if err != nil {
				reason = "device unplugged"
			}
			s.log.Warn("USB device lost", "usb", key, "busid", busid, "reason", reason)
			s.dropUSB(ctx, m, rt, key)
			if err != nil {
				s.setUSB(key, errors.New(reason), usbip.Device{})
				failed = true
				continue
			}
		}
		dev, err := w.usb[key].spec.Find(devs)
		if err == nil {
			err = s.attachUSB(ctx, m, rt, dev)
		}
		if prev := s.setUSB(key, err, dev); err != nil {
			if prev != err.Error() {
				s.log.Warn("USB attach failed", "usb", key, "err", err)
			}
			failed = true
			continue
		}
		s.log.Info("USB device attached", "usb", key, "busid", dev.BusID, "name", dev.Name)
		rt.attached[key] = dev.BusID
	}
	return failed
}

func (s *session) attachUSB(ctx context.Context, m *openssh.Master, rt *usbRuntime, dev usbip.Device) error {
	ctx, cancel := context.WithTimeout(ctx, mountStartTimeout)
	defer cancel()
	if dev.Status == usbip.StatusUsed {
		// The polled list may predate its release, e.g. by a host it was
		// just moved from.
		if fresh, err := usbip.Read(dev.BusID); err != nil || fresh.Status == usbip.StatusUsed {
			return fmt.Errorf("%s is attached elsewhere", dev.BusID)
		}
	}
	port, err := s.usb.Port(ctx)
	if err != nil {
		return err
	}
	if rt.port == 0 {
		spec := forward.Spec{
			Kind:   forward.Remote,
			Listen: forward.Endpoint{Host: "127.0.0.1"},
			Target: forward.Endpoint{Host: "127.0.0.1", Port: port},
		}
		rport, err := m.Forward(ctx, spec)
		if err != nil {
			return fmt.Errorf("forwarding the USB/IP port: %w", err)
		}
		rt.fwd, rt.port = spec, rport
		// Cancelling needs the port the server picked.
		rt.fwd.Listen.Port = rport
	}
	if err := s.usb.Export(ctx, dev.BusID); err != nil {
		return err
	}
	if _, err := m.Run(ctx, "", usbip.RemoteCommand(usbip.AttachScript, strconv.Itoa(rt.port), dev.BusID)); err != nil {
		s.usb.Unexport(context.WithoutCancel(ctx), dev.BusID)
		return usbip.RemoteError(err)
	}
	// usbip attach returns once the remote asked; the device is ours once
	// the kernel here reports it in use.
	deadline := time.Now().Add(usbAttachWait)
	for time.Now().Before(deadline) {
		if d, err := usbip.Read(dev.BusID); err == nil && d.Status == usbip.StatusUsed {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.usb.Unexport(context.WithoutCancel(ctx), dev.BusID)
	return errors.New("the remote attached the device, but it never came into use")
}

// detachUSB detaches a device on the remote and gives it back here.
func (s *session) detachUSB(ctx context.Context, m *openssh.Master, rt *usbRuntime, key string) {
	busid := rt.attached[key]
	s.dropUSB(ctx, m, rt, key)
	s.log.Info("USB device detached", "usb", key, "busid", busid)
}

// dropUSB forgets an attachment: detaches it on the remote if it's still
// there, and unexports it.
func (s *session) dropUSB(ctx context.Context, m *openssh.Master, rt *usbRuntime, key string) {
	busid := rt.attached[key]
	delete(rt.attached, key)
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	if m != nil {
		if out, err := m.Run(ctx, "", usbip.RemoteCommand(usbip.PortScript)); err != nil {
			s.log.Warn("listing remote USB ports failed", "err", usbip.RemoteError(err))
		} else if port, ok := usbip.FindPort(string(out), rt.port, busid); ok {
			if _, err := m.Run(ctx, "", usbip.RemoteCommand(usbip.DetachScript, strconv.Itoa(port))); err != nil {
				s.log.Warn("remote USB detach failed", "busid", busid, "err", usbip.RemoteError(err))
			}
		}
	}
	if err := s.usb.Unexport(ctx, busid); err != nil {
		s.log.Warn("giving the USB device back failed", "busid", busid, "err", err)
	}
}

// stopUSB detaches everything: on the remote while m is still connected,
// else just here. The forward goes with the connection.
func (s *session) stopUSB(m *openssh.Master, rt *usbRuntime) {
	for _, key := range slices.Sorted(maps.Keys(rt.attached)) {
		s.detachUSB(context.Background(), m, rt, key)
	}
	rt.port = 0
}

// setUSB records the outcome of attaching and returns the previous error.
func (s *session) setUSB(key string, err error, dev usbip.Device) (prevErr string) {
	s.mu.Lock()
	if us := s.usbStates[key]; us != nil {
		prevErr = us.err
		if err != nil {
			*us = usbState{state: api.StateError, err: err.Error()}
		} else {
			*us = usbState{state: api.StateUp, busid: dev.BusID, name: dev.Name}
		}
	}
	s.mu.Unlock()
	s.changed()
	return prevErr
}

// usbStatuses reports shared devices for status. Callers hold s.mu.
func (s *session) usbStatuses() []api.USBStatus {
	out := []api.USBStatus{}
	for _, key := range slices.Sorted(maps.Keys(s.usbStates)) {
		us, wu := s.usbStates[key], s.want.usb[key]
		out = append(out, api.USBStatus{
			Device:   key,
			BusID:    us.busid,
			Name:     us.name,
			Profiles: wu.profiles,
			AdHoc:    wu.adhoc,
			State:    us.state,
			Error:    us.err,
		})
	}
	return out
}
