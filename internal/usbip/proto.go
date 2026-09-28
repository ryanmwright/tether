package usbip

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// The USB/IP setup protocol (Documentation/usb/usbip_protocol.rst): the
// client asks for the device list or to import a device; after a successful
// import the connection belongs to the kernel. Everything is big-endian.
const (
	protoVersion = 0x0111

	opReqDevlist = 0x8005
	opRepDevlist = 0x0005
	opReqImport  = 0x8003
	opRepImport  = 0x0003

	stOK    = 0
	stNA    = 1 // not available
	stNoDev = 4

	busIDSize  = 32
	pathSize   = 256
	deviceSize = pathSize + busIDSize + 3*4 + 3*2 + 6
)

type opCommon struct {
	Version uint16
	Code    uint16
	Status  uint32
}

func readOp(r io.Reader) (opCommon, error) {
	var op opCommon
	err := binary.Read(r, binary.BigEndian, &op)
	return op, err
}

func writeOp(w io.Writer, code uint16, status uint32) error {
	return binary.Write(w, binary.BigEndian, opCommon{protoVersion, code, status})
}

// cString is s as a fixed-size, NUL-padded field.
func cString(s string, size int) []byte {
	b := make([]byte, size)
	copy(b[:size-1], s)
	return b
}

func parseCString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// encodeDevice is struct usbip_usb_device.
func encodeDevice(d Device) []byte {
	var b bytes.Buffer
	b.Write(cString(d.Path, pathSize))
	b.Write(cString(d.BusID, busIDSize))
	var vendor, product uint16
	fmt.Sscanf(d.Vendor, "%04x", &vendor)
	fmt.Sscanf(d.Product, "%04x", &product)
	binary.Write(&b, binary.BigEndian, struct {
		BusNum, DevNum, Speed                  uint32
		Vendor, Product, BcdDevice             uint16
		Class, SubClass, Protocol              uint8
		ConfigValue, NumConfigs, NumInterfaces uint8
	}{d.BusNum, d.DevNum, d.Speed, vendor, product, d.BcdDevice, d.Class, d.SubClass, d.Protocol, d.ConfigValue, d.NumConfigs, d.NumInterfaces})
	return b.Bytes()
}

// encodeDevlist is the reply to a device list request.
func encodeDevlist(devs []Device) []byte {
	var b bytes.Buffer
	writeOp(&b, opRepDevlist, stOK)
	binary.Write(&b, binary.BigEndian, uint32(len(devs)))
	for _, d := range devs {
		b.Write(encodeDevice(d))
		for _, i := range d.Interfaces {
			b.Write([]byte{i[0], i[1], i[2], 0})
		}
	}
	return b.Bytes()
}
