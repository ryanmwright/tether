package usbip

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultHelperSocket is where the helper listens for the daemon.
const DefaultHelperSocket = "/run/tether-usbip/helper.sock"

// DefaultListen is the helper's USB/IP address. Only loopback makes sense:
// remotes reach it through a reverse forward.
const DefaultListen = "127.0.0.1:3240"

const setupTimeout = 10 * time.Second

type HelperOptions struct {
	Socket string // control socket for the daemon
	Listen string // USB/IP address, host:port on loopback
	// AllowUIDs may use the helper. Each user can only reach the devices it
	// exported.
	AllowUIDs []int
	Log       *slog.Logger
}

// request and response are the control protocol: one JSON object per line.
type request struct {
	Op    string `json:"op"` // hello, export, unexport
	BusID string `json:"busid,omitempty"`
}

type response struct {
	Error string `json:"error,omitempty"`
	Port  int    `json:"port,omitempty"` // for hello: the USB/IP port
}

type export struct {
	uid   int
	owner *controlConn
}

type helper struct {
	opts HelperOptions
	log  *slog.Logger
	port int

	mu      sync.Mutex
	exports map[string]*export // bus ID -> who exported it
}

type controlConn struct{ uid int }

// RunHelper serves until ctx is cancelled. It must run as root. Devices are
// exported for as long as the control connection that asked for them stays
// open, so they come back to this machine if the daemon goes away.
func RunHelper(ctx context.Context, opts HelperOptions) error {
	if os.Geteuid() != 0 {
		return errors.New("the USB/IP helper must run as root")
	}
	host, _, err := net.SplitHostPort(opts.Listen)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() || ip.To4() == nil {
		return fmt.Errorf("--listen must be an IPv4 loopback address, not %q", host)
	}
	h := &helper{opts: opts, log: opts.Log, exports: map[string]*export{}}
	if err := loadModule(); err != nil {
		h.log.Warn("usbip-host kernel module unavailable; exporting will fail", "err", err)
	}

	tl, err := net.Listen("tcp4", opts.Listen)
	if err != nil {
		return fmt.Errorf("USB/IP port: %w (is usbipd running?)", err)
	}
	defer tl.Close()
	h.port = tl.Addr().(*net.TCPAddr).Port

	if err := os.MkdirAll(filepath.Dir(opts.Socket), 0o755); err != nil {
		return err
	}
	os.Remove(opts.Socket)
	ul, err := net.Listen("unix", opts.Socket)
	if err != nil {
		return err
	}
	defer os.Remove(opts.Socket)
	defer ul.Close()
	// Anyone may connect; the peer's uid is checked on accept.
	if err := os.Chmod(opts.Socket, 0o666); err != nil {
		return err
	}

	go func() {
		<-ctx.Done()
		tl.Close()
		ul.Close()
	}()
	defer h.unexportAll()
	h.log.Info("USB/IP helper started", "socket", opts.Socket, "listen", tl.Addr(), "users", opts.AllowUIDs)

	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go h.serveUSBIP(c.(*net.TCPConn))
		}
	})
	for {
		c, err := ul.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Go(func() { h.serveControl(ctx, c.(*net.UnixConn)) })
	}
}

func (h *helper) allowed(uid int) bool {
	return uid == 0 || slices.Contains(h.opts.AllowUIDs, uid)
}

func (h *helper) serveControl(ctx context.Context, c *net.UnixConn) {
	defer c.Close()
	uid, err := unixPeerUID(c)
	if err != nil || !h.allowed(uid) {
		h.log.Warn("control connection refused", "uid", uid, "err", err)
		json.NewEncoder(c).Encode(response{Error: fmt.Sprintf("uid %d may not use the USB/IP helper; add it with --allow-user", uid)})
		return
	}
	cc := &controlConn{uid: uid}
	defer h.release(cc)
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()

	sc := bufio.NewScanner(c)
	enc := json.NewEncoder(c)
	for sc.Scan() {
		var req request
		var resp response
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			resp.Error = "invalid request: " + err.Error()
		} else if err := h.handle(cc, req, &resp); err != nil {
			resp.Error = err.Error()
		}
		if enc.Encode(resp) != nil {
			return
		}
	}
}

func (h *helper) handle(cc *controlConn, req request, resp *response) error {
	switch req.Op {
	case "hello":
		resp.Port = h.port
		return nil
	case "export":
		return h.export(cc, req.BusID)
	case "unexport":
		return h.unexport(cc, req.BusID)
	}
	return fmt.Errorf("unknown op %q", req.Op)
}

func (h *helper) export(cc *controlConn, busid string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.exports[busid]; e != nil {
		if e.owner != cc {
			return fmt.Errorf("%s is already shared by another tether", busid)
		}
		if d, err := Read(busid); err == nil && d.Driver == stubDriver {
			return nil
		}
	}
	if err := bind(busid); err != nil {
		return err
	}
	h.exports[busid] = &export{uid: cc.uid, owner: cc}
	h.log.Info("exported", "busid", busid, "uid", cc.uid)
	return nil
}

func (h *helper) unexport(cc *controlConn, busid string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.exports[busid]
	if e == nil {
		return nil
	}
	if e.owner != cc {
		return fmt.Errorf("%s is shared by another tether", busid)
	}
	delete(h.exports, busid)
	h.log.Info("unexported", "busid", busid)
	return unbind(busid)
}

// release gives back everything a control connection exported.
func (h *helper) release(cc *controlConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for busid, e := range h.exports {
		if e.owner == cc {
			delete(h.exports, busid)
			if err := unbind(busid); err != nil {
				h.log.Warn("unexport failed", "busid", busid, "err", err)
			} else {
				h.log.Info("unexported", "busid", busid, "reason", "daemon disconnected")
			}
		}
	}
}

func (h *helper) unexportAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for busid := range h.exports {
		unbind(busid)
		delete(h.exports, busid)
	}
}

// serveUSBIP answers one USB/IP client: a device list, or an import that
// hands the connection to the kernel.
func (h *helper) serveUSBIP(c *net.TCPConn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(setupTimeout))
	uid, err := tcpPeerUID(c)
	if err != nil || !h.allowed(uid) {
		h.log.Warn("USB/IP connection refused", "peer", c.RemoteAddr(), "uid", uid, "err", err)
		return
	}
	op, err := readOp(c)
	if err != nil {
		return
	}
	switch op.Code {
	case opReqDevlist:
		var devs []Device
		h.mu.Lock()
		for busid, e := range h.exports {
			if d, err := Read(busid); err == nil && (e.uid == uid || uid == 0) {
				devs = append(devs, d)
			}
		}
		h.mu.Unlock()
		c.Write(encodeDevlist(devs))
	case opReqImport:
		buf := make([]byte, busIDSize)
		if _, err := io.ReadFull(c, buf); err != nil {
			return
		}
		busid := parseCString(buf)
		d, status := h.importDevice(c, uid, busid)
		var reply []byte
		reply = binary.BigEndian.AppendUint16(reply, protoVersion)
		reply = binary.BigEndian.AppendUint16(reply, opRepImport)
		reply = binary.BigEndian.AppendUint32(reply, status)
		if status == stOK {
			reply = append(reply, encodeDevice(d)...)
		}
		c.Write(reply)
	default:
		h.log.Warn("unknown USB/IP request", "code", fmt.Sprintf("%#04x", op.Code))
	}
}

// importDevice gives the connection to the kernel's stub driver for busid.
func (h *helper) importDevice(c *net.TCPConn, uid int, busid string) (Device, uint32) {
	h.mu.Lock()
	e := h.exports[busid]
	h.mu.Unlock()
	if e == nil || e.uid != uid && uid != 0 {
		h.log.Warn("import of a device not shared", "busid", busid, "uid", uid)
		return Device{}, stNoDev
	}
	d, err := Read(busid)
	if err != nil || d.Status != StatusAvailable {
		h.log.Warn("import of an unavailable device", "busid", busid, "status", d.Status, "err", err)
		return d, stNA
	}
	c.SetKeepAlive(true)
	f, err := c.File()
	if err != nil {
		return d, stNA
	}
	defer f.Close()
	// The kernel looks the descriptor up in the writing process, i.e. us.
	sockfd := filepath.Join(devicesDir(), busid, "usbip_sockfd")
	if err := os.WriteFile(sockfd, []byte(strconv.Itoa(int(f.Fd()))+"\n"), 0); err != nil {
		h.log.Warn("handing the connection to the kernel failed", "busid", busid, "err", err)
		return d, stNA
	}
	h.log.Info("device attached", "busid", busid, "peer", c.RemoteAddr())
	return d, stOK
}

func driversDir() string { return filepath.Join(SysfsRoot, "bus/usb/drivers") }

func writeAttr(path, value string) error {
	if err := os.WriteFile(path, []byte(value), 0); err != nil {
		return fmt.Errorf("writing %q to %s: %w", value, path, unwrapPath(err))
	}
	return nil
}

func unwrapPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

func loadModule() error {
	if _, err := os.Stat(filepath.Join(driversDir(), stubDriver)); err == nil {
		return nil
	}
	if out, err := exec.Command("modprobe", "usbip-host").CombinedOutput(); err != nil {
		return fmt.Errorf("modprobe usbip-host: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// bind moves a device from its driver to usbip-host, like `usbip bind`.
func bind(busid string) error {
	d, err := Read(busid)
	if err != nil {
		return err
	}
	if d.Class == 0x09 {
		return fmt.Errorf("%s is a hub; share the devices behind it instead", busid)
	}
	if d.Driver == stubDriver {
		return nil
	}
	if err := loadModule(); err != nil {
		return err
	}
	stub := filepath.Join(driversDir(), stubDriver)
	if d.Driver != "" {
		if err := writeAttr(filepath.Join(devicesDir(), busid, "driver/unbind"), busid); err != nil {
			return err
		}
	}
	if err := writeAttr(filepath.Join(stub, "match_busid"), "add "+busid); err != nil {
		return err
	}
	if err := writeAttr(filepath.Join(stub, "bind"), busid); err != nil {
		writeAttr(filepath.Join(stub, "match_busid"), "del "+busid)
		writeAttr(filepath.Join(stub, "rebind"), busid)
		return err
	}
	return nil
}

// unbind gives a device back to its usual driver, like `usbip unbind`.
func unbind(busid string) error {
	stub := filepath.Join(driversDir(), stubDriver)
	d, err := Read(busid)
	if err != nil {
		writeAttr(filepath.Join(stub, "match_busid"), "del "+busid)
		return nil // unplugged
	}
	if d.Driver == stubDriver {
		if err := writeAttr(filepath.Join(stub, "unbind"), busid); err != nil {
			return err
		}
	}
	if err := writeAttr(filepath.Join(stub, "match_busid"), "del "+busid); err != nil {
		return err
	}
	return writeAttr(filepath.Join(stub, "rebind"), busid)
}

func unixPeerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return -1, err
	}
	if cerr != nil {
		return -1, cerr
	}
	return int(cred.Uid), nil
}

// tcpPeerUID finds who owns the other end of a loopback connection, from
// /proc/net/tcp.
func tcpPeerUID(c *net.TCPConn) (int, error) {
	f, err := os.Open("/proc/net/tcp")
	if err != nil {
		return -1, err
	}
	defer f.Close()
	return findSocketUID(f, c.RemoteAddr().(*net.TCPAddr), c.LocalAddr().(*net.TCPAddr))
}

// findSocketUID looks up the socket from local to remote in a
// /proc/net/tcp table.
func findSocketUID(table io.Reader, local, remote *net.TCPAddr) (int, error) {
	want := procAddr(local) + " " + procAddr(remote)
	sc := bufio.NewScanner(table)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) > 7 && f[1]+" "+f[2] == want {
			return strconv.Atoi(f[7])
		}
	}
	return -1, fmt.Errorf("no socket %s in /proc/net/tcp", want)
}

// procAddr formats an IPv4 address as /proc/net/tcp does: the address as a
// little-endian word, then the port, in hex.
func procAddr(a *net.TCPAddr) string {
	ip := a.IP.To4()
	if ip == nil {
		return "?"
	}
	return fmt.Sprintf("%02X%02X%02X%02X:%04X", ip[3], ip[2], ip[1], ip[0], a.Port)
}
