package dbus

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Message types from the D-Bus specification.
const (
	TypeMethodCall  = 1
	TypeMethodReply = 2
	TypeError       = 3
	TypeSignal      = 4
)

// Header field codes from the D-Bus specification.
const (
	fieldPath        = 1
	fieldInterface   = 2
	fieldMember      = 3
	fieldErrorName   = 4
	fieldReplySerial = 5
	fieldDestination = 6
	fieldSignature   = 8
)

// DefaultSystemBus is the usual address of the system bus socket.
const DefaultSystemBus = "unix:path=/run/dbus/system_bus_socket"

// Conn is one connection to a D-Bus daemon. It is safe for concurrent use.
type Conn struct {
	mu     sync.Mutex
	sock   net.Conn
	r      *bufio.Reader
	serial uint32
	// uniqueName is the address the bus assigned in reply to Hello, for
	// example ":1.42". It is only useful in logs.
	uniqueName string
	// Timeout bounds every method call.
	Timeout time.Duration
	closed  bool
}

// Dial connects and authenticates to a bus. address may be empty, in which
// case DBUS_SYSTEM_BUS_ADDRESS or the standard system socket is used.
func Dial(address string) (*Conn, error) {
	if address == "" {
		address = os.Getenv("DBUS_SYSTEM_BUS_ADDRESS")
	}
	if address == "" {
		address = DefaultSystemBus
	}
	network, target, err := parseAddress(address)
	if err != nil {
		return nil, err
	}
	sock, err := net.DialTimeout(network, target, 5*time.Second)
	if err != nil {
		return nil, err
	}
	c := &Conn{sock: sock, r: bufio.NewReaderSize(sock, 64*1024), Timeout: 10 * time.Second}
	if err := c.auth(); err != nil {
		sock.Close()
		return nil, err
	}
	if err := c.hello(); err != nil {
		sock.Close()
		return nil, err
	}
	return c, nil
}

// hello introduces the client to the bus, which is not optional.
//
// The first message a client sends to a bus must be org.freedesktop.DBus.Hello;
// a bus that receives anything else first is required by the specification
// to disconnect the sender. The failure mode is hard to read from the
// outside, because the disconnect arrives as an empty read on a later call
// rather than as an error on the offending one.
func (c *Conn) hello() error {
	var uniqueName string
	err := c.Call("org.freedesktop.DBus", "/org/freedesktop/DBus",
		"org.freedesktop.DBus", "Hello", "", nil, &uniqueName)
	if err != nil {
		return fmt.Errorf("dbus: the bus refused the client introduction: %w", err)
	}
	c.mu.Lock()
	c.uniqueName = uniqueName
	c.mu.Unlock()
	return nil
}

// parseAddress understands the unix:path= and unix:abstract= forms, which
// are the only transports used for the system bus.
func parseAddress(addr string) (network, target string, err error) {
	for _, part := range strings.Split(addr, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kind, kv, ok := strings.Cut(part, ":")
		if !ok || kind != "unix" {
			continue
		}
		for _, pair := range strings.Split(kv, ",") {
			k, v, ok := strings.Cut(pair, "=")
			if !ok {
				continue
			}
			switch k {
			case "path":
				return "unix", v, nil
			case "abstract":
				// A leading NUL marks the abstract namespace.
				return "unix", "\x00" + v, nil
			}
		}
	}
	return "", "", fmt.Errorf("dbus: unsupported bus address %q", addr)
}

// auth performs the SASL EXTERNAL handshake, sending the caller's UID.
func (c *Conn) auth() error {
	uid := strconvItoa(os.Getuid())
	hexUID := make([]byte, 0, len(uid)*2)
	for i := 0; i < len(uid); i++ {
		hexUID = append(hexUID, hexDigits[uid[i]>>4], hexDigits[uid[i]&0xf])
	}
	if _, err := c.sock.Write([]byte("\x00AUTH EXTERNAL " + string(hexUID) + "\r\n")); err != nil {
		return err
	}
	line, err := c.readLine()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, "OK ") {
		return fmt.Errorf("dbus: authentication rejected: %s", line)
	}
	// Unix file descriptor passing is deliberately not negotiated. Nothing
	// in this client sends or receives a descriptor, and once a bus has
	// agreed to fd passing it expects a UNIX_FDS header field on every
	// message that follows. Not asking is simpler and has fewer ways to
	// go wrong.
	if _, err := c.sock.Write([]byte("BEGIN\r\n")); err != nil {
		return err
	}
	return nil
}

const hexDigits = "0123456789abcdef"

func (c *Conn) readLine() (string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// Close shuts the connection down.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.sock.Close()
}

// Call performs a method call and returns the reply body.
//
// args are the values of bodySig; this client only supports the basic
// types, which is all systemd's manager interface needs for the calls the
// panel makes. out may be nil, or a pointer to a value matching bodySig.
func (c *Conn) Call(dest, path, iface, member, bodySig string, args []any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("dbus: connection is closed")
	}
	c.serial++
	serial := c.serial
	if err := c.sock.SetDeadline(time.Now().Add(c.Timeout)); err != nil {
		return err
	}
	msg, err := buildCall(serial, dest, path, iface, member, bodySig, args)
	if err != nil {
		return err
	}
	if _, err := c.sock.Write(msg); err != nil {
		return err
	}
	for {
		fields, body, err := c.readMessage()
		if err != nil {
			return err
		}
		// Ignore signals and replies to other calls.
		if rs, _ := fields[fieldReplySerial].(uint32); rs != serial {
			continue
		}
		if name, ok := fields[fieldErrorName].(string); ok && name != "" {
			return Error{Name: name, Body: body}
		}
		if out == nil || len(body) == 0 {
			return nil
		}
		// The reply carries its own signature, and it is not necessarily
		// the shape of the request. Hello is the clearest example: the
		// call takes no arguments and answers with a string, so decoding
		// the reply with the request's signature fails on a message that
		// is perfectly well formed.
		replySig, _ := fields[fieldSignature].(string)
		if replySig == "" {
			return nil
		}
		d := &decoder{buf: body}
		return d.unmarshalValue(replySig, out)
	}
}

// Error is a D-Bus error reply.
type Error struct {
	Name string
	Body []byte
}

func (e Error) Error() string { return "dbus: remote error " + e.Name }

// buildCall assembles a complete method call message.
func buildCall(serial uint32, dest, path, iface, member, bodySig string, args []any) ([]byte, error) {
	headerFields := []headerField{
		{fieldPath, ObjectPath(path)},
		{fieldInterface, iface},
		{fieldMember, member},
		{fieldDestination, dest},
	}
	if bodySig != "" {
		headerFields = append(headerFields, headerField{fieldSignature, Signature(bodySig)})
	}
	header := encodeHeaderFields(headerFields)

	// The body begins at the next 8 byte boundary after the header field
	// array, which itself starts at offset 16. Sixteen is a multiple of
	// eight, so the array's own length is enough to work out where the
	// body lands.
	bodyStart := 16 + alignTo(len(header), 8)
	body, err := MarshalBody(bodyStart, bodySig, args)
	if err != nil {
		return nil, err
	}

	fixed := make([]byte, 16)
	fixed[0] = 'l'
	fixed[1] = TypeMethodCall
	fixed[2] = 0 // no flags
	fixed[3] = 1 // protocol version
	// The fixed header carries two separate lengths, and confusing them
	// makes the daemon drop the connection without a word: the body length
	// counts the body alone, and the field array length tells the daemon
	// where that body begins.
	binary.LittleEndian.PutUint32(fixed[4:], uint32(len(body)))
	binary.LittleEndian.PutUint32(fixed[8:], serial)
	binary.LittleEndian.PutUint32(fixed[12:], uint32(len(header)))

	msg := make([]byte, 0, bodyStart+len(body))
	msg = append(msg, fixed...)
	msg = append(msg, header...)
	// Padding between the array and the body. Without it the daemon reads
	// the body from the wrong offset.
	for len(msg) < bodyStart {
		msg = append(msg, 0)
	}
	msg = append(msg, body...)
	return msg, nil
}

type headerField struct {
	code  byte
	value any
}

// encodeHeaderFields writes the a(yv) header field array.
func encodeHeaderFields(fs []headerField) []byte {
	// Only the contents of the array are returned, without a length
	// prefix. In a real message that prefix is the field at offset 12 of
	// the fixed header, which is to say the array begins there rather than
	// after it. Writing a second prefix here shifts the whole message by
	// eight bytes and the daemon drops the connection.
	//
	// The length of what is returned is a byte count, because a D-Bus
	// array measures its contents in bytes and not in elements.
	e := &encoder{}
	for _, f := range fs {
		e.align(8)
		e.writeByte(f.code)
		switch v := f.value.(type) {
		case ObjectPath:
			e.writeSignature("o")
			e.writeString(string(v))
		case Signature:
			e.writeSignature("g")
			e.writeSignature(string(v))
		default:
			e.writeSignature("s")
			e.writeString(fmt.Sprint(v))
		}
	}
	return e.buf
}

// readMessage reads one complete message from the wire.
func (c *Conn) readMessage() (map[byte]any, []byte, error) {
	// The first sixteen bytes: twelve of message metadata followed by the
	// length of the header field array, whose contents begin at byte 16.
	fixed := make([]byte, 16)
	if _, err := io.ReadFull(c.r, fixed); err != nil {
		return nil, nil, err
	}
	if fixed[0] != 'l' {
		return nil, nil, fmt.Errorf("dbus: unsupported endianness byte %q", fixed[0])
	}
	bodyLen := int(binary.LittleEndian.Uint32(fixed[4:]))
	fieldsLen := int(binary.LittleEndian.Uint32(fixed[12:]))
	if bodyLen < 0 || fieldsLen < 0 || bodyLen > 64<<20 || fieldsLen > 16<<20 {
		return nil, nil, errors.New("dbus: implausible message length")
	}
	// The array contents are padded to an 8 byte boundary before the body,
	// so what follows the fixed header is longer than the two lengths
	// added together whenever the contents do not end on one.
	padded := alignTo(fieldsLen, 8)
	rest := make([]byte, padded+bodyLen)
	if _, err := io.ReadFull(c.r, rest); err != nil {
		return nil, nil, err
	}
	fieldsBuf := rest[:fieldsLen]
	body := rest[padded:]

	// The contents are a bare sequence of (yv) structs: the array framing
	// was the length field in the fixed header, which has already been
	// consumed, so there is no second prefix to read here.
	fields := make(map[byte]any, 8)
	if fieldsLen > 0 {
		d := &decoder{buf: fieldsBuf}
		for d.pos < len(d.buf) {
			var f struct {
				Code  byte
				Value any
			}
			if err := d.unmarshalValue("(yv)", &f); err != nil {
				return nil, nil, err
			}
			switch f.Code {
			case fieldPath, fieldInterface, fieldMember, fieldErrorName, fieldDestination, fieldSignature:
				fields[f.Code] = stringOf(f.Value)
			case fieldReplySerial:
				fields[f.Code] = uint32Of(f.Value)
			}
		}
	}
	return fields, body, nil
}

// alignTo rounds n up to the next multiple of a.
func alignTo(n, a int) int {
	if n%a == 0 {
		return n
	}
	return n + a - n%a
}

func stringOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case ObjectPath:
		return string(t)
	case Signature:
		return string(t)
	}
	return fmt.Sprint(v)
}

func uint32Of(v any) uint32 {
	switch t := v.(type) {
	case uint32:
		return t
	case uint64:
		return uint32(t)
	case int:
		return uint32(t)
	case uint16:
		return uint32(t)
	case byte:
		return uint32(t)
	}
	return 0
}

func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// SortStrings sorts a copy of the given slice, used for stable output.
func SortStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
