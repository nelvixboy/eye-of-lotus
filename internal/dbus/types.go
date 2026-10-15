// Package dbus is a minimal D-Bus client, just enough to talk to
// org.freedesktop.systemd1 over the system bus.
//
// It exists because Eye of Lotus should not depend on systemctl being
// installed, and because the maintained D-Bus libraries for Go pull in a
// dependency tree this project does not need. Only the pieces the panel
// uses are implemented: the SASL EXTERNAL handshake, method calls with
// marshalled arguments, and replies.
package dbus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
)

// Alignment returns the byte alignment required by a D-Bus type code.
func Alignment(sig byte) int {
	switch sig {
	case 'y', 'g', 'v':
		return 1
	case 'n', 'q', 'i', 'u', 'b', 'h':
		return 4
	default:
		return 8
	}
}

// isBasic reports whether a type code is a fixed-size basic type.
func isBasic(sig byte) bool {
	return strings.IndexByte("ybnqiuxtdshog", sig) >= 0
}

// sigChar returns the signature character for a Go value, used when the
// caller did not supply one explicitly.
func sigChar(v any) (byte, error) {
	switch v.(type) {
	case byte:
		return 'y', nil
	case bool:
		return 'b', nil
	case int16:
		return 'n', nil
	case uint16:
		return 'q', nil
	case int32, int:
		return 'i', nil
	case uint32, uint:
		return 'u', nil
	case int64:
		return 'x', nil
	case uint64:
		return 't', nil
	case float64:
		return 'd', nil
	case string:
		return 's', nil
	case ObjectPath:
		return 'o', nil
	case Signature:
		return 'g', nil
	}
	return 0, fmt.Errorf("dbus: cannot derive signature for %T", v)
}

// ObjectPath is a D-Bus object path.
type ObjectPath string

// Signature is a D-Bus type signature.
type Signature string

type encoder struct {
	buf []byte
}

func (e *encoder) align(n int) {
	for len(e.buf)%n != 0 {
		e.buf = append(e.buf, 0)
	}
}

func (e *encoder) writeByte(b byte) { e.buf = append(e.buf, b) }

func (e *encoder) writeUint16(v uint16) {
	e.align(2)
	e.buf = binary.LittleEndian.AppendUint16(e.buf, v)
}

func (e *encoder) writeUint32(v uint32) {
	e.align(4)
	e.buf = binary.LittleEndian.AppendUint32(e.buf, v)
}

func (e *encoder) writeUint64(v uint64) {
	e.align(8)
	e.buf = binary.LittleEndian.AppendUint64(e.buf, v)
}

// writeString writes the D-Bus string format: a uint32 length, the bytes,
// and a terminating NUL that is not counted in the length.
func (e *encoder) writeString(s string) {
	e.writeUint32(uint32(len(s)))
	e.buf = append(e.buf, s...)
	e.buf = append(e.buf, 0)
}

func (e *encoder) writeSignature(sig string) {
	e.writeByte(byte(len(sig)))
	e.buf = append(e.buf, sig...)
	e.buf = append(e.buf, 0)
}

// marshalValue encodes one Go value using the D-Bus type code.
func (e *encoder) marshalValue(sig byte, v any) error {
	switch sig {
	case 'y':
		e.writeByte(byte(toUint(v)))
	case 'b':
		if toBool(v) {
			e.writeUint32(1)
		} else {
			e.writeUint32(0)
		}
	case 'n':
		e.writeUint16(uint16(int16(toInt(v))))
	case 'q':
		e.writeUint16(uint16(toUint(v)))
	case 'i':
		e.writeUint32(uint32(int32(toInt(v))))
	case 'u':
		e.writeUint32(uint32(toUint(v)))
	case 'x':
		e.writeUint64(uint64(toInt(v)))
	case 't':
		e.writeUint64(toUint(v))
	case 'd':
		e.writeUint64(math.Float64bits(toFloat(v)))
	case 's':
		e.writeString(toStr(v))
	case 'o':
		e.writeString(toStr(v))
	case 'g':
		e.writeSignature(toStr(v))
	case 'v':
		inner, err := sigChar(v)
		if err != nil {
			return err
		}
		e.writeSignature(string(inner))
		return e.marshalValue(inner, v)
	case 'a':
		return errors.New("dbus: array needs an element signature")
	}
	return nil
}

func toUint(v any) uint64 {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return uint64(rv.Int())
	case reflect.Bool:
		if rv.Bool() {
			return 1
		}
	}
	return 0
}

func toInt(v any) int64 {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(rv.Uint())
	}
	return 0
}

func toFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return float64(toInt(v))
}

func toBool(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return toUint(v) != 0
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if s, ok := v.(ObjectPath); ok {
		return string(s)
	}
	if s, ok := v.(Signature); ok {
		return string(s)
	}
	return fmt.Sprint(v)
}

// MarshalBody encodes a method body given its signature and the argument
// values. offset is the byte position at which the body starts within the
// message, which matters because D-Bus padding is relative to the message,
// not to the body.
func MarshalBody(offset int, sig string, args []any) ([]byte, error) {
	e := &encoder{buf: make([]byte, offset)}
	pos := 0
	for _, c := range []byte(sig) {
		if c == 'a' {
			return nil, errors.New("dbus: MarshalBody does not support arrays")
		}
		if pos >= len(args) {
			return nil, fmt.Errorf("dbus: signature %q has more types than arguments", sig)
		}
		if err := e.marshalValue(c, args[pos]); err != nil {
			return nil, err
		}
		pos++
	}
	return e.buf[offset:], nil
}

// decoder reads D-Bus wire data.
type decoder struct {
	buf []byte
	pos int
}

func (d *decoder) align(n int) error {
	for d.pos%n != 0 {
		if d.pos >= len(d.buf) {
			return errors.New("dbus: truncated message while aligning")
		}
		d.pos++
	}
	return nil
}

func (d *decoder) need(n int) error {
	if d.pos+n > len(d.buf) {
		return fmt.Errorf("dbus: truncated message: want %d bytes at %d, have %d", n, d.pos, len(d.buf)-d.pos)
	}
	return nil
}

func (d *decoder) readByte() (byte, error) {
	if err := d.need(1); err != nil {
		return 0, err
	}
	b := d.buf[d.pos]
	d.pos++
	return b, nil
}

func (d *decoder) readUint16() (uint16, error) {
	if err := d.align(2); err != nil {
		return 0, err
	}
	if err := d.need(2); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint16(d.buf[d.pos:])
	d.pos += 2
	return v, nil
}

func (d *decoder) readUint32() (uint32, error) {
	if err := d.align(4); err != nil {
		return 0, err
	}
	if err := d.need(4); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint32(d.buf[d.pos:])
	d.pos += 4
	return v, nil
}

func (d *decoder) readUint64() (uint64, error) {
	if err := d.align(8); err != nil {
		return 0, err
	}
	if err := d.need(8); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint64(d.buf[d.pos:])
	d.pos += 8
	return v, nil
}

func (d *decoder) readString() (string, error) {
	n, err := d.readUint32()
	if err != nil {
		return "", err
	}
	if err := d.need(int(n) + 1); err != nil {
		return "", err
	}
	s := string(d.buf[d.pos : d.pos+int(n)])
	d.pos += int(n) + 1
	return s, nil
}

func (d *decoder) readSignature() (string, error) {
	n, err := d.readByte()
	if err != nil {
		return "", err
	}
	if err := d.need(int(n) + 1); err != nil {
		return "", err
	}
	s := string(d.buf[d.pos : d.pos+int(n)])
	d.pos += int(n) + 1
	return s, nil
}

// skipTo advances the cursor past a value of the given type without
// decoding it. It is used for signature features this client ignores.
func (d *decoder) skipTo(sig string) error {
	for _, c := range []byte(sig) {
		if err := d.skipOne(c); err != nil {
			return err
		}
	}
	return nil
}

func (d *decoder) skipOne(c byte) error {
	switch c {
	case 'y':
		_, err := d.readByte()
		return err
	case 'b', 'i', 'u':
		_, err := d.readUint32()
		return err
	case 'n', 'q':
		_, err := d.readUint16()
		return err
	case 'x', 't', 'd':
		_, err := d.readUint64()
		return err
	case 's', 'o':
		_, err := d.readString()
		return err
	case 'g':
		_, err := d.readSignature()
		return err
	case 'v':
		s, err := d.readSignature()
		if err != nil {
			return err
		}
		return d.skipTo(s)
	case 'a':
		return errors.New("dbus: skip of array needs a concrete element type")
	}
	return fmt.Errorf("dbus: cannot skip type %q", string(c))
}

// elementSig returns the signature of the elements of an array type, and
// the remainder of the signature after the array.
func elementSig(sig string) (elem string, rest string, err error) {
	if len(sig) < 2 || sig[0] != 'a' {
		return "", "", fmt.Errorf("dbus: %q is not an array signature", sig)
	}
	switch sig[1] {
	case 'a':
		inner, rest, err := elementSig(sig[1:])
		if err != nil {
			return "", "", err
		}
		return "a" + inner, rest, nil
	case '(':
		depth := 0
		for i := 1; i < len(sig); i++ {
			switch sig[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					return sig[1 : i+1], sig[i+1:], nil
				}
			}
		}
		return "", "", errors.New("dbus: unbalanced struct signature")
	case '{':
		depth := 0
		for i := 1; i < len(sig); i++ {
			switch sig[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return sig[1 : i+1], sig[i+1:], nil
				}
			}
		}
		return "", "", errors.New("dbus: unbalanced dict signature")
	default:
		return sig[1:2], sig[2:], nil
	}
}

// splitStructMembers splits the inside of a struct signature into members.
func splitStructMembers(sig string) []string {
	var out []string
	for i := 0; i < len(sig); {
		switch sig[i] {
		case 'a':
			elem, rest, err := elementSig(sig[i:])
			if err != nil {
				return append(out, sig[i:])
			}
			out = append(out, "a"+elem)
			i = len(sig) - len(rest)
		case '(':
			elem, rest, err := elementSig("a" + sig[i:])
			if err != nil {
				return append(out, sig[i:])
			}
			out = append(out, elem)
			i = len(sig) - len(rest)
		default:
			out = append(out, sig[i:i+1])
			i++
		}
	}
	return out
}
