package controlproto

import (
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// THE WIRE SHAPE IS PINNED TO THE WIRE VERSION (MGIT-250). Both directions
// decode with DisallowUnknownFields, so an added request or response field
// breaks a peer one version behind: the rule is to bump ProtocolVersion in the
// same commit (handshake.go). TestProtocolVersion_IsPinned stops an accidental
// bump; this stops the opposite, a shape change with no bump. MGIT-231 added
// last_boot_failure to SandboxInfo, which rides in Response two types down,
// and every test passed until the bump was noticed by reading handshake.go.
//
// The shape is derived by reflection from Request and Response through every
// nested struct, pointer, slice and map, so the case list comes from the types
// themselves and not from a list this test keeps. Each field is recorded as
// its JSON path, name, ",string" option and Go kind: a rename, an added or
// removed field, a changed type or a changed encoding moves the digest. Request kinds are byte constants and carry no
// shape; a new verb still needs its own bump by the rule in handshake.go.
//
// WHEN THIS FAILS: the wire shape changed. Bump ProtocolVersion, record why in
// handshake.go's history and TestProtocolVersion_IsPinned, and add the new
// version's digest below (the failure prints it). Never edit an existing
// entry: a digest recorded for a version is what that version spoke.
// Refs: MGIT-250, MGIT-231, MGIT-136
var wireShapeDigests = map[int]string{
	5: "44b0d5cc5d5864ff8909d342c04dcd3b413be3eb014565f6ea7f7536cac9e2a3",
}

func TestWireShape_IsPinnedToProtocolVersion(t *testing.T) {
	got := wireShapeDigest(wireShape(reflect.TypeOf(Request{}), reflect.TypeOf(Response{})))

	want, ok := wireShapeDigests[ProtocolVersion]
	require.True(t, ok, "ProtocolVersion %d has no recorded wire shape; add %d: %q to wireShapeDigests",
		ProtocolVersion, ProtocolVersion, got)
	assert.Equal(t, want, got,
		"the control-protocol wire shape changed without a ProtocolVersion bump (still %d). "+
			"Bump it in the same commit, say why in handshake.go, and record %d: %q in wireShapeDigests",
		ProtocolVersion, ProtocolVersion+1, got)
}

// The walk must reach nested types: a digest over the top-level fields alone
// would let a field added two types down (the MGIT-231 shape) pass. Two
// envelopes that differ only in a nested type's field must differ.
func TestWireShape_SeesAFieldAddedTwoTypesDown(t *testing.T) {
	type innerV1 struct {
		State string `json:"state"`
	}
	type innerV2 struct {
		State       string `json:"state"`
		LastFailure string `json:"last_failure,omitempty"`
	}
	type envelopeV1 struct {
		List []innerV1 `json:"list,omitempty"`
	}
	type envelopeV2 struct {
		List []innerV2 `json:"list,omitempty"`
	}

	v1 := wireShape(reflect.TypeOf(envelopeV1{}))
	v2 := wireShape(reflect.TypeOf(envelopeV2{}))

	assert.NotEqual(t, wireShapeDigest(v1), wireShapeDigest(v2))
	assert.Contains(t, v2, "0.list[].last_failure string")
}

// A ",string" option quotes a number on the wire, so adding or removing it is
// a shape change; omitempty is not.
func TestWireShape_StringOptionIsShapeOmitemptyIsNot(t *testing.T) {
	type plain struct {
		Port int `json:"port"`
	}
	type quoted struct {
		Port int `json:"port,string"`
	}
	type omitted struct {
		Port int `json:"port,omitempty"`
	}

	p := wireShapeDigest(wireShape(reflect.TypeOf(plain{})))

	assert.NotEqual(t, p, wireShapeDigest(wireShape(reflect.TypeOf(quoted{}))))
	assert.Equal(t, p, wireShapeDigest(wireShape(reflect.TypeOf(omitted{}))))
}

// The real shape reaches the MGIT-231 field, so the pin above covers it.
func TestWireShape_ReachesSandboxInfoInsideResponse(t *testing.T) {
	shape := wireShape(reflect.TypeOf(Request{}), reflect.TypeOf(Response{}))
	assert.Contains(t, shape, "1.sandbox.last_boot_failure.cause string")
	assert.Contains(t, shape, "1.list[].last_boot_failure.cause string")
}

var (
	jsonMarshaler = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshaler = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// wireShape lists every JSON field reachable from the roots, one line per
// field: "<root index>.<json path> <kind>". A type with its own JSON or text
// encoding is a leaf, recorded by its Go type name. Recursion stops at a type
// already on the current path.
func wireShape(roots ...reflect.Type) []string {
	var out []string
	for i, root := range roots {
		walkWireType(root, string(rune('0'+i)), map[reflect.Type]bool{}, &out)
	}
	sort.Strings(out)
	return out
}

func walkWireType(t reflect.Type, path string, onPath map[reflect.Type]bool, out *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t.Implements(jsonMarshaler) || reflect.PointerTo(t).Implements(jsonMarshaler) ||
		t.Implements(textMarshaler) || reflect.PointerTo(t).Implements(textMarshaler):
		*out = append(*out, path+" "+t.String())
		return
	case t.Kind() == reflect.Slice || t.Kind() == reflect.Array:
		walkWireType(t.Elem(), path+"[]", onPath, out)
		return
	case t.Kind() == reflect.Map:
		walkWireType(t.Elem(), path+"{"+t.Key().Kind().String()+"}", onPath, out)
		return
	case t.Kind() != reflect.Struct:
		*out = append(*out, path+" "+t.Kind().String())
		return
	}
	if onPath[t] {
		*out = append(*out, path+" recursive "+t.String())
		return
	}
	onPath[t] = true
	defer delete(onPath, t)
	for i := 0; i < t.NumField(); i++ {
		walkWireField(t.Field(i), path, onPath, out)
	}
}

func walkWireField(f reflect.StructField, path string, onPath map[reflect.Type]bool, out *[]string) {
	tag := f.Tag.Get("json")
	if tag == "-" || (!f.IsExported() && !f.Anonymous) {
		return
	}
	name, opts, _ := strings.Cut(tag, ",")
	if f.Anonymous && name == "" {
		walkWireType(f.Type, path, onPath, out) // encoding/json inlines an untagged embedded struct
		return
	}
	if name == "" {
		name = f.Name
	}
	// ",string" changes how a number or bool is encoded (quoted), so it is part
	// of the shape; omitempty only decides whether a zero value is sent.
	for _, o := range strings.Split(opts, ",") {
		if o == "string" {
			name += ",string"
		}
	}
	walkWireType(f.Type, path+"."+name, onPath, out)
}

func wireShapeDigest(shape []string) string {
	sum := sha256.Sum256([]byte(strings.Join(shape, "\n")))
	return hex.EncodeToString(sum[:])
}
