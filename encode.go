package fixedwidth

import (
	"bufio"
	"bytes"
	"encoding"
	"io"
	"reflect"
	"strconv"
	"sync"
	"unicode/utf8"
)

// Marshal returns the fixed-width encoding of v.
//
// v must be an encodable type or a slice of an encodable
// type. If v is a slice, each item will be treated as a
// line. If v is a single encodable type, a single line
// will be encoded.
//
// In order for a type to be encodable, it must implement
// the encoding.TextMarshaler interface or be based on one
// of the following builtin types: string, int, int64,
// int32, int16, int8, float64, float32, or struct.
// Pointers to encodable types and interfaces containing
// encodable types are also encodable.
//
// nil pointers and interfaces will be omitted. zero vales
// will be encoded normally.
//
// A struct is encoded to a single slice of bytes. Each
// field in a struct will be encoded and placed at the
// position defined by its struct tags. The tags should be
// formatted as `fixed:"{startPos},{endPos}"`. Positions
// start at 1. The interval is inclusive. Fields without
// tags and Fields of an un-encodable type are ignored.
//
// If the encoded value of a field is longer than the
// length of the position interval, the overflow is
// truncated.
func Marshal(v interface{}) ([]byte, error) {
	buff := bytes.NewBuffer(nil)
	err := NewEncoder(buff).Encode(v)
	if err != nil {
		return nil, err
	}
	return buff.Bytes(), nil
}

// MarshalInvalidTypeError describes an invalid type being marshaled.
type MarshalInvalidTypeError struct {
	typeName string
}

func (e *MarshalInvalidTypeError) Error() string {
	return "fixedwidth: cannot marshal unknown Type " + e.typeName
}

// An Encoder writes fixed-width formatted data to an output
// stream.
type Encoder struct {
	w *bufio.Writer
}

// NewEncoder returns a new encoder that writes to w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{
		bufio.NewWriter(w),
	}
}

// Encode writes the fixed-width encoding of v to the
// stream.
// See the documentation for Marshal for details about
// encoding behavior.
func (e *Encoder) Encode(i interface{}) (err error) {
	if i == nil {
		return nil
	}

	// check to see if i should be encoded into multiple lines
	v := reflect.ValueOf(i)
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		v = v.Elem()
	}
	if v.Kind() == reflect.Slice {
		// encode each slice element to a line
		err = e.writeLines(v)
	} else {
		// this is a single object so encode the original vale to a line
		err = e.writeLine(reflect.ValueOf(i))
	}
	if err != nil {
		return err
	}
	return e.w.Flush()
}

func (e *Encoder) writeLines(v reflect.Value) error {
	for i := 0; i < v.Len(); i++ {
		err := e.writeLine(v.Index(i))
		if err != nil {
			return err
		}

		if i != v.Len()-1 {
			_, err := e.w.Write([]byte("\n"))
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Encoder) writeLine(v reflect.Value) (err error) {
	b, err := newValueEncoder(v.Type())(v)
	if err != nil {
		return err
	}
	_, err = e.w.Write(b)
	return err
}

type valueEncoder func(v reflect.Value) ([]byte, error)

func newValueEncoder(t reflect.Type) valueEncoder {
	if t == nil {
		return nilEncoder
	}
	if t.Implements(reflect.TypeOf(new(encoding.TextMarshaler)).Elem()) {
		return textMarshalerEncoder
	}

	switch t.Kind() {
	case reflect.Ptr, reflect.Interface:
		return ptrInterfaceEncoder
	case reflect.Struct:
		return structEncoder
	case reflect.String:
		return stringEncoder
	case reflect.Int, reflect.Int64, reflect.Int32, reflect.Int16, reflect.Int8:
		return intEncoder
	case reflect.Float64:
		return floatEncoder(2, 64)
	case reflect.Float32:
		return floatEncoder(2, 32)
	}
	return unknownTypeEncoder(t)
}

func structEncoder(v reflect.Value) ([]byte, error) {
	ss := cachedStructSpec(v.Type())
	dst := bytes.Repeat([]byte(" "), ss.ll*utf8.UTFMax)
	size := 0

	for i, spec := range ss.fieldSpecs {
		if !spec.ok {
			continue
		}

		f := v.Field(i)
		val, err := newValueEncoder(f.Type())(f)
		if err != nil {
			return nil, err
		}

		val, padding := getValidChunk(val, f.Kind(), spec.startPos, spec.endPos)
		copy(dst[size:size+len(val)], val)
		size += len(val) + padding
	}

	return dst[:size], nil
}

// getValidChunk gets the valid chunk from field value base on start and end position
// number of additional spaces will be returned if the chunk doesn't fullfill defined storage
func getValidChunk(val []byte, kind reflect.Kind, startPos, endPos int) ([]byte, int) {
	if endPos < startPos {
		return val, 0
	}

	numberOfRunes := endPos - startPos + 1

	if kindIsNumber(kind) {
		return val, numberOfRunes - len(val)
	}

	temp := val
	size := 0
	numberOfPadding := 0

	for numberOfRunes > 0 {
		if len(temp) == 0 {
			numberOfPadding++
			numberOfRunes--
			continue
		}

		_, s := utf8.DecodeRune(temp)
		size += s
		temp = temp[s:]
		numberOfRunes--
	}

	return val[:size], numberOfPadding
}

// kindIsNumber check if kind k is a number (int, uint, ...)
func kindIsNumber(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int64, reflect.Int32, reflect.Int16, reflect.Int8, reflect.Float32, reflect.Float64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	default:
		return false
	}
}

type structSpec struct {
	ll         int
	fieldSpecs []fieldSpec
}

type fieldSpec struct {
	startPos, endPos int
	ok               bool
}

func buildStructSpec(t reflect.Type) structSpec {
	ss := structSpec{
		fieldSpecs: make([]fieldSpec, t.NumField()),
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		ss.fieldSpecs[i].startPos, ss.fieldSpecs[i].endPos, ss.fieldSpecs[i].ok = parseTag(f.Tag.Get("fixed"))
		if ss.fieldSpecs[i].endPos > ss.ll {
			ss.ll = ss.fieldSpecs[i].endPos
		}
	}
	return ss
}

var fieldSpecCache sync.Map // map[reflect.Type]structSpec

// cachedStructSpec is like buildStructSpec but cached to prevent duplicate work.
func cachedStructSpec(t reflect.Type) structSpec {
	if f, ok := fieldSpecCache.Load(t); ok {
		return f.(structSpec)
	}
	f, _ := fieldSpecCache.LoadOrStore(t, buildStructSpec(t))
	return f.(structSpec)
}

func textMarshalerEncoder(v reflect.Value) ([]byte, error) {
	return v.Interface().(encoding.TextMarshaler).MarshalText()
}

func ptrInterfaceEncoder(v reflect.Value) ([]byte, error) {
	if v.IsNil() {
		return nilEncoder(v)
	}
	return newValueEncoder(v.Elem().Type())(v.Elem())
}

func stringEncoder(v reflect.Value) ([]byte, error) {
	return []byte(v.String()), nil
}

func intEncoder(v reflect.Value) ([]byte, error) {
	return []byte(strconv.Itoa(int(v.Int()))), nil
}

func floatEncoder(perc, bitSize int) valueEncoder {
	return func(v reflect.Value) ([]byte, error) {
		return []byte(strconv.FormatFloat(v.Float(), 'f', perc, bitSize)), nil
	}
}

func nilEncoder(v reflect.Value) ([]byte, error) {
	return nil, nil
}

func unknownTypeEncoder(t reflect.Type) valueEncoder {
	return func(value reflect.Value) ([]byte, error) {
		return nil, &MarshalInvalidTypeError{typeName: t.Name()}
	}
}
