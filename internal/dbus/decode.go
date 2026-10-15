package dbus

import (
	"errors"
	"fmt"
	"math"
	"reflect"
)

// unmarshalValue decodes one value of the given type and stores it in dst,
// which must be a pointer. Unknown or unsupported types are skipped so that
// a richer remote interface never breaks the client.
func (d *decoder) unmarshalValue(sig string, dst any) error {
	if len(sig) == 0 {
		return errors.New("dbus: empty signature")
	}
	switch sig[0] {
	case 'a':
		return d.unmarshalArray(sig, dst)
	case '(':
		return d.unmarshalStruct(sig, dst)
	case 'v':
		inner, err := d.readSignature()
		if err != nil {
			return err
		}
		return d.unmarshalValue(inner, dst)
	case '{':
		// A dict entry outside an array: skip it.
		return d.skipOne('a')
	default:
		return d.unmarshalBasic(sig[0], dst)
	}
}

func (d *decoder) unmarshalBasic(c byte, dst any) error {
	switch c {
	case 'y':
		v, err := d.readByte()
		if err != nil {
			return err
		}
		setUint(dst, uint64(v))
	case 'b':
		v, err := d.readUint32()
		if err != nil {
			return err
		}
		setBool(dst, v != 0)
	case 'n':
		v, err := d.readUint16()
		if err != nil {
			return err
		}
		setInt(dst, int64(int16(v)))
	case 'q':
		v, err := d.readUint16()
		if err != nil {
			return err
		}
		setUint(dst, uint64(v))
	case 'i':
		v, err := d.readUint32()
		if err != nil {
			return err
		}
		setInt(dst, int64(int32(v)))
	case 'u':
		v, err := d.readUint32()
		if err != nil {
			return err
		}
		setUint(dst, uint64(v))
	case 'x':
		v, err := d.readUint64()
		if err != nil {
			return err
		}
		setInt(dst, int64(v))
	case 't':
		v, err := d.readUint64()
		if err != nil {
			return err
		}
		setUint(dst, v)
	case 'd':
		v, err := d.readUint64()
		if err != nil {
			return err
		}
		setFloat(dst, float64frombits(v))
	case 's', 'o':
		v, err := d.readString()
		if err != nil {
			return err
		}
		setString(dst, v)
	case 'g':
		v, err := d.readSignature()
		if err != nil {
			return err
		}
		setString(dst, v)
	default:
		return fmt.Errorf("dbus: cannot decode type %q", string(c))
	}
	return nil
}

func (d *decoder) unmarshalArray(sig string, dst any) error {
	elem, _, err := elementSig(sig)
	if err != nil {
		return err
	}
	n, err := d.readUint32()
	if err != nil {
		return err
	}
	// Arrays of structs and dict entries are padded to 8 bytes.
	if elem[0] == '(' || elem[0] == '{' {
		if err := d.align(8); err != nil {
			return err
		}
	}
	end := d.pos + int(n)
	if end > len(d.buf) {
		return fmt.Errorf("dbus: array length %d exceeds message", n)
	}
	holder, err := newSliceFor(dst, elem)
	if err != nil {
		// No destination: skip the array.
		d.pos = end
		return nil
	}
	for d.pos < end {
		item, err := holder.newElem()
		if err != nil {
			return err
		}
		if err := d.unmarshalValue(elem, item.ptr); err != nil {
			return err
		}
		holder.append(item.ptr)
	}
	holder.assign()
	return nil
}

// unmarshalStruct decodes a struct into a Go struct by position, but only
// where the destination field can actually hold the member. A member the
// destination cannot accept is skipped rather than written into the wrong
// field.
//
// That matters because remote interfaces grow. systemd's ListUnits gained
// a "following" string in the middle of its struct, and a client that
// assigns strictly by index silently ends up with the object path in the
// wrong field and empty properties everywhere. Matching on type as well as
// position makes the client survive that.
func (d *decoder) unmarshalStruct(sig string, dst any) error {
	if err := d.align(8); err != nil {
		return err
	}
	members := splitStructMembers(sig[1 : len(sig)-1])
	fields := structTargets(dst)

	next := 0
	for _, m := range members {
		if next < len(fields) && compatible(m, fields[next].ptr) {
			if err := d.unmarshalValue(m, fields[next].ptr); err != nil {
				return err
			}
			next++
			continue
		}
		// Either the destination has no room left, or this member does
		// not fit the next field: skip it and keep looking.
		if err := d.skipMember(m); err != nil {
			return err
		}
	}
	return nil
}

// compatible reports whether a destination can hold a value of the given
// D-Bus type. The check is deliberately strict about the named D-Bus types
// so that a struct member cannot land in the wrong field.
func compatible(sig string, dst any) bool {
	if len(sig) == 0 || dst == nil {
		return false
	}
	// A variant carries its own real type, so anything can receive one.
	if sig[0] == 'v' {
		return true
	}
	switch sig[0] {
	case 's':
		_, ok := dst.(*string)
		return ok
	case 'o':
		_, ok := dst.(*ObjectPath)
		return ok
	case 'g':
		_, ok := dst.(*Signature)
		return ok
	case 'b':
		_, ok := dst.(*bool)
		return ok
	case 'd':
		_, ok := dst.(*float64)
		return ok
	case 'y', 'q', 'u', 't':
		switch dst.(type) {
		case *uint8, *uint16, *uint32, *uint64:
			return true
		}
		return false
	case 'n', 'i', 'x':
		switch dst.(type) {
		case *int16, *int32, *int64, *int:
			return true
		}
		return false
	case 'a':
		rv := reflect.ValueOf(dst)
		return rv.Kind() == reflect.Pointer && !rv.IsNil() && rv.Elem().Kind() == reflect.Slice
	case '(':
		rv := reflect.ValueOf(dst)
		return rv.Kind() == reflect.Pointer && !rv.IsNil() && rv.Elem().Kind() == reflect.Struct
	}
	return false
}

// skipMember skips a single member of any supported type.
func (d *decoder) skipMember(sig string) error {
	if sig[0] == 'a' {
		elem, _, err := elementSig(sig)
		if err != nil {
			return err
		}
		n, err := d.readUint32()
		if err != nil {
			return err
		}
		if elem[0] == '(' || elem[0] == '{' {
			if err := d.align(8); err != nil {
				return err
			}
		}
		d.pos += int(n)
		if d.pos > len(d.buf) {
			return errors.New("dbus: array skip past end of message")
		}
		return nil
	}
	if sig[0] == '(' {
		if err := d.align(8); err != nil {
			return err
		}
		for _, m := range splitStructMembers(sig[1 : len(sig)-1]) {
			if err := d.skipMember(m); err != nil {
				return err
			}
		}
		return nil
	}
	if sig[0] == 'v' {
		inner, err := d.readSignature()
		if err != nil {
			return err
		}
		return d.skipMember(inner)
	}
	return d.unmarshalValue(sig, nil)
}

func float64frombits(v uint64) float64 {
	return math.Float64frombits(v)
}

// target is a settable destination for one decoded value.
type target struct {
	ptr any
}

// sliceHolder accumulates array elements into a destination slice.
type sliceHolder struct {
	dst      reflect.Value
	items    reflect.Value
	elemType reflect.Type
}

// newElem allocates a fresh element of the slice kind held by dst.
func (h *sliceHolder) newElem() (target, error) {
	v := reflect.New(h.elemType)
	return target{ptr: v.Interface()}, nil
}

func (h *sliceHolder) append(v any) {
	h.items = reflect.Append(h.items, reflect.ValueOf(v).Elem())
}

func (h *sliceHolder) assign() { h.dst.Set(h.items) }

// newSliceFor prepares a destination slice for an array of elemSig.
func newSliceFor(dst any, elemSig string) (*sliceHolder, error) {
	if dst == nil {
		return nil, errors.New("dbus: no destination for array")
	}
	dv := reflect.ValueOf(dst)
	if dv.Kind() != reflect.Pointer || dv.IsNil() || dv.Elem().Kind() != reflect.Slice {
		return nil, fmt.Errorf("dbus: destination %T is not a pointer to a slice", dst)
	}
	sl := dv.Elem()
	return &sliceHolder{dst: sl, items: reflect.MakeSlice(sl.Type(), 0, 8), elemType: sl.Type().Elem()}, nil
}

// structTargets returns pointers to the fields of a struct destination.
func structTargets(dst any) []target {
	if dst == nil {
		return nil
	}
	dv := reflect.ValueOf(dst)
	if dv.Kind() != reflect.Pointer || dv.IsNil() || dv.Elem().Kind() != reflect.Struct {
		return nil
	}
	st := dv.Elem()
	out := make([]target, 0, st.NumField())
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if !f.CanAddr() || !f.CanSet() {
			continue
		}
		out = append(out, target{ptr: f.Addr().Interface()})
	}
	return out
}

func setUint(dst any, v uint64) {
	if p, ok := dst.(*uint64); ok {
		*p = v
		return
	}
	if p, ok := dst.(*uint32); ok {
		*p = uint32(v)
		return
	}
	if p, ok := dst.(*uint16); ok {
		*p = uint16(v)
		return
	}
	if p, ok := dst.(*uint8); ok {
		*p = uint8(v)
		return
	}
	if p, ok := dst.(*byte); ok {
		*p = byte(v)
		return
	}
	if p, ok := dst.(*int); ok {
		*p = int(v)
		return
	}
	if p, ok := dst.(*int64); ok {
		*p = int64(v)
	}
}

func setInt(dst any, v int64) {
	if p, ok := dst.(*int64); ok {
		*p = v
		return
	}
	if p, ok := dst.(*int32); ok {
		*p = int32(v)
		return
	}
	if p, ok := dst.(*int); ok {
		*p = int(v)
		return
	}
	if p, ok := dst.(*int16); ok {
		*p = int16(v)
		return
	}
	if p, ok := dst.(*uint64); ok {
		*p = uint64(v)
	}
}

func setString(dst any, v string) {
	if p, ok := dst.(*string); ok {
		*p = v
		return
	}
	if p, ok := dst.(*ObjectPath); ok {
		*p = ObjectPath(v)
		return
	}
	if p, ok := dst.(*Signature); ok {
		*p = Signature(v)
	}
}

func setBool(dst any, v bool) {
	if p, ok := dst.(*bool); ok {
		*p = v
	}
}

func setFloat(dst any, v float64) {
	if p, ok := dst.(*float64); ok {
		*p = v
	}
}
