package dbus

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"testing"
)

func TestAlignment(t *testing.T) {
	cases := map[byte]int{'y': 1, 'g': 1, 'v': 1, 'n': 4, 'q': 4, 'i': 4, 'u': 4, 'b': 4,
		's': 8, 'o': 8, 'a': 8, 'x': 8, 't': 8, 'd': 8, '(': 8, '{': 8}
	for sig, want := range cases {
		if got := Alignment(sig); got != want {
			t.Errorf("Alignment(%q) = %d, want %d", string(sig), got, want)
		}
	}
}

func TestSplitStructMembers(t *testing.T) {
	cases := map[string][]string{
		"ssssssouso": {"s", "s", "s", "s", "s", "s", "o", "u", "s", "o"},
		"sa{sv}":     {"s", "a{sv}"},
		"a(ss)u":     {"a(ss)", "u"},
		"yvu":        {"y", "v", "u"},
	}
	for sig, want := range cases {
		got := splitStructMembers(sig)
		if len(got) != len(want) {
			t.Errorf("splitStructMembers(%q) = %v, want %v", sig, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("splitStructMembers(%q) = %v, want %v", sig, got, want)
				break
			}
		}
	}
}

func TestElementSig(t *testing.T) {
	cases := []struct {
		sig, elem, rest string
	}{
		{"a(ssssssouso)", "(ssssssouso)", ""},
		{"a{sv}", "{sv}", ""},
		{"as", "s", ""},
		{"aai", "ai", ""},
		{"a(ss)u", "(ss)", "u"},
	}
	for _, tc := range cases {
		elem, rest, err := elementSig(tc.sig)
		if err != nil {
			t.Errorf("elementSig(%q): %v", tc.sig, err)
			continue
		}
		if elem != tc.elem || rest != tc.rest {
			t.Errorf("elementSig(%q) = (%q, %q), want (%q, %q)", tc.sig, elem, rest, tc.elem, tc.rest)
		}
	}
	if _, _, err := elementSig("ss"); err == nil {
		t.Error("elementSig should reject a non-array signature")
	}
}

// TestMarshalBodyString checks the wire format of a string argument, which
// is the shape of almost every systemd property call: a uint32 length, the
// bytes, and a terminating NUL that is not counted in the length.
func TestMarshalBodyString(t *testing.T) {
	body, err := MarshalBody(16, "ss", []any{"nginx.service", "UnitFileState"})
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	// "nginx.service" is 13 bytes.
	if got := binary.LittleEndian.Uint32(body[0:4]); got != 13 {
		t.Errorf("first string length = %d, want 13", got)
	}
	if string(body[4:17]) != "nginx.service" {
		t.Errorf("first string bytes = %q", string(body[4:17]))
	}
	if body[17] != 0 {
		t.Errorf("first string is not NUL terminated: %d", body[17])
	}
	// The second string starts at the next 4 byte boundary after 18 bytes.
	if got := binary.LittleEndian.Uint32(body[20:24]); got != 13 {
		t.Errorf("second string length = %d, want 13 (offset 20)", got)
	}
	if string(body[24:37]) != "UnitFileState" {
		t.Errorf("second string bytes = %q", string(body[24:37]))
	}
}

// TestMarshalBodyAlignment checks that padding is relative to the start of
// the whole message rather than to the body, which is the detail that
// makes hand written D-Bus encoders fail.
func TestMarshalBodyAlignment(t *testing.T) {
	// With a body offset of 17, a uint32 must be padded by 3 bytes to land
	// on byte 20.
	body, err := MarshalBody(17, "u", []any{uint32(7)})
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if len(body) != 7 {
		t.Fatalf("body length = %d, want 7 (3 padding bytes plus a uint32)", len(body))
	}
	for i := 0; i < 3; i++ {
		if body[i] != 0 {
			t.Errorf("padding byte %d is %d, want 0", i, body[i])
		}
	}
	if got := binary.LittleEndian.Uint32(body[3:7]); got != 7 {
		t.Errorf("value = %d, want 7", got)
	}
}

// TestMarshalBodyVariant checks the variant encoding used for header
// fields, where the value is preceded by its own signature.
func TestMarshalBodyVariant(t *testing.T) {
	// A variant derives its contained type from the Go value, so the
	// signature is a bare "v" rather than "vu".
	body, err := MarshalBody(0, "v", []any{uint32(42)})
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if body[0] != 1 || body[1] != 'u' {
		t.Errorf("variant signature = %q, want a one byte %q", string(body[:2]), "u")
	}
	// The payload starts at the next 4 byte boundary.
	if got := binary.LittleEndian.Uint32(body[4:8]); got != 42 {
		t.Errorf("variant value = %d, want 42", got)
	}
}

func TestMarshalBodyRejectsWrongArgumentCount(t *testing.T) {
	if _, err := MarshalBody(0, "ss", []any{"only one"}); err == nil {
		t.Error("a signature with more types than arguments should be an error")
	}
	if _, err := MarshalBody(0, "a(ss)", []any{nil}); err == nil {
		t.Error("an array body should be rejected with a clear error")
	}
}

// TestDecodeReplyBody decodes a body the encoder produced, which proves
// the two halves of the client agree with each other.
func TestDecodeReplyBody(t *testing.T) {
	body, err := MarshalBody(0, "s", []any{"systemd"})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	d := &decoder{buf: body}
	if err := d.unmarshalValue("s", &got); err != nil {
		t.Fatalf("unmarshalValue: %v", err)
	}
	if got != "systemd" {
		t.Errorf("decoded %q, want %q", got, "systemd")
	}
}

// TestDecodeArrayOfStructs mirrors the reply of systemd's ListUnits, whose
// array of ten member structs is the most demanding shape the client
// handles.
func TestDecodeArrayOfStructs(t *testing.T) {
	// One entry, in the shape systemd's ListUnits replies with: name,
	// description, load, active, sub, following, path, job id, job type,
	// job path. The destination struct is deliberately shorter than the
	// signature, because skipping members a caller does not care about is
	// exactly what has to keep working when systemd adds fields.
	// The array is built by the encoder itself so that the length written
	// into the header is the length the decoder will verify.
	entry := &encoder{}
	entry.align(8)
	entry.writeString("nginx.service")
	entry.writeString("A high performance web server")
	entry.writeString("loaded")
	entry.writeString("active")
	entry.writeString("running")
	// The sixth string is the "following" unit, which this client ignores
	// but which the wire format still carries.
	entry.writeString("")
	entry.writeString("/org/freedesktop/systemd1/unit/nginx_2eservice")
	entry.writeUint32(0)
	entry.writeString("")
	entry.writeString("/")

	body := &encoder{}
	body.writeUint32(uint32(len(entry.buf)))
	body.align(8)
	body.buf = append(body.buf, entry.buf...)

	type unit struct {
		Name        string
		Description string
		LoadState   string
		ActiveState string
		SubState    string
		Path        ObjectPath
		JobID       uint32
		JobType     string
		JobPath     ObjectPath
	}
	var units []unit
	d := &decoder{buf: body.buf}
	if err := d.unmarshalValue("a(ssssssouso)", &units); err != nil {
		t.Fatalf("unmarshalValue: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("decoded %d units, want 1", len(units))
	}
	u := units[0]
	if u.Name != "nginx.service" || u.ActiveState != "active" || u.SubState != "running" {
		t.Errorf("unit fields wrong: %+v", u)
	}
	if string(u.Path) != "/org/freedesktop/systemd1/unit/nginx_2eservice" {
		t.Errorf("object path = %q", u.Path)
	}
	if u.JobID != 0 || u.JobType != "" || string(u.JobPath) != "/" {
		t.Errorf("job fields wrong: %+v", u)
	}
}

func TestMarshalBodyVariantOfString(t *testing.T) {
	body, err := MarshalBody(0, "v", []any{ObjectPath("/org/freedesktop/systemd1")})
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if body[0] != 1 || body[1] != 'o' {
		t.Errorf("variant signature = %q, want %q", string(body[:2]), "o")
	}
	var got ObjectPath
	d := &decoder{buf: body}
	if err := d.unmarshalValue("v", &got); err != nil {
		t.Fatalf("unmarshalValue: %v", err)
	}
	if string(got) != "/org/freedesktop/systemd1" {
		t.Errorf("decoded %q", got)
	}
}

func TestParseAddress(t *testing.T) {
	network, target, err := parseAddress("unix:path=/run/dbus/system_bus_socket")
	if err != nil || network != "unix" || target != "/run/dbus/system_bus_socket" {
		t.Errorf("path form parsed as (%q, %q, %v)", network, target, err)
	}
	network, target, err = parseAddress("unix:abstract=/tmp/dbus-abc,guid=1234")
	if err != nil || network != "unix" || target != "\x00/tmp/dbus-abc" {
		t.Errorf("abstract form parsed as (%q, %q, %v)", network, target, err)
	}
	if _, _, err := parseAddress("tcp:host=localhost,port=1234"); err == nil {
		t.Error("an unsupported transport should be an error")
	}
}

// TestBuildCallRoundTrip feeds a freshly built method call back through the
// client's own reader. This is the check that would have caught the two
// length fields in the fixed header being wrong, since a real daemon drops
// the connection over them and this reader fails in the same place.
func TestBuildCallRoundTrip(t *testing.T) {
	msg, err := buildCall(7,
		"org.freedesktop.systemd1",
		"/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager",
		"GetVersionString", "", nil)
	if err != nil {
		t.Fatalf("buildCall: %v", err)
	}
	if msg[0] != 'l' || msg[1] != TypeMethodCall {
		t.Fatalf("fixed header is wrong: endianness %q type %d", msg[0], msg[1])
	}
	if got := binary.LittleEndian.Uint32(msg[4:8]); got != 0 {
		t.Errorf("body length = %d for a call without arguments, want 0", got)
	}
	if got := binary.LittleEndian.Uint32(msg[8:12]); got != 7 {
		t.Errorf("serial = %d, want 7", got)
	}
	fieldsLen := int(binary.LittleEndian.Uint32(msg[12:16]))
	if fieldsLen == 0 {
		t.Fatal("header field array length is zero: a daemon would drop the connection")
	}
	if 16+alignTo(fieldsLen, 8) != len(msg) {
		t.Errorf("lengths do not add up: 16 + %d + 0 != %d", alignTo(fieldsLen, 8), len(msg))
	}

	c := &Conn{r: bufio.NewReader(bytes.NewReader(msg))}
	fields, _, err := c.readMessage()
	if err != nil {
		t.Fatalf("readMessage on a message this package built: %v", err)
	}
	want := map[byte]string{
		fieldPath:        "/org/freedesktop/systemd1",
		fieldInterface:   "org.freedesktop.systemd1.Manager",
		fieldMember:      "GetVersionString",
		fieldDestination: "org.freedesktop.systemd1",
	}
	for code, value := range want {
		if got := fields[code]; got != value {
			t.Errorf("header field %d = %v, want %q", code, got, value)
		}
	}
}

// TestBuildCallWithArguments checks the invariant that matters once a body
// is present: the two lengths in the fixed header, plus the padding before
// the body, must account for every byte after the fixed header. A D-Bus
// array records its size in bytes, so the header array length is a byte
// count and not the number of fields.
func TestBuildCallWithArguments(t *testing.T) {
	msg, err := buildCall(9,
		"org.freedesktop.systemd1",
		"/org/freedesktop/systemd1/unit/nginx_2eservice",
		"org.freedesktop.DBus.Properties",
		"Get", "ss",
		[]any{"org.freedesktop.systemd1.Unit", "UnitFileState"})
	if err != nil {
		t.Fatalf("buildCall: %v", err)
	}
	bodyLen := int(binary.LittleEndian.Uint32(msg[4:8]))
	fieldsLen := int(binary.LittleEndian.Uint32(msg[12:16]))
	if bodyLen == 0 {
		t.Error("body length is zero for a call that carries arguments")
	}
	if fieldsLen == 0 {
		t.Error("header field array length is zero")
	}
	if 16+alignTo(fieldsLen, 8)+bodyLen != len(msg) {
		t.Errorf("lengths do not add up: 16 + %d + %d != %d",
			alignTo(fieldsLen, 8), bodyLen, len(msg))
	}

	// The body has to decode from the reader's slice without help, which
	// is only true when that slice starts on an 8 byte boundary.
	c := &Conn{r: bufio.NewReader(bytes.NewReader(msg))}
	_, body, err := c.readMessage()
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	d := &decoder{buf: body}
	var iface, name string
	if err := d.unmarshalValue("s", &iface); err != nil {
		t.Fatalf("decode first argument: %v", err)
	}
	if err := d.unmarshalValue("s", &name); err != nil {
		t.Fatalf("decode second argument: %v", err)
	}
	if iface != "org.freedesktop.systemd1.Unit" || name != "UnitFileState" {
		t.Errorf("arguments decoded as %q, %q", iface, name)
	}
}
