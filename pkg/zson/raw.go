package zson

import (
	"errors"
	"fmt"

	"github.com/buger/jsonparser"
	"github.com/mccanne/zq/pkg/nano"
	"github.com/mccanne/zq/pkg/zeek"
	"github.com/mccanne/zq/pkg/zval"
)

// NewRawFromZvals builds a raw value from a descriptor and zvals.
func NewRawFromZvals(d *Descriptor, vals [][]byte) (zval.Encoding, error) {
	if nv, nc := len(vals), len(d.Type.Columns); nv != nc {
		return nil, fmt.Errorf("got %d values (%q), expected %d (%q)", nv, vals, nc, d.Type.Columns)

	}
	var raw zval.Encoding
	for _, val := range vals {
		raw = zval.AppendValue(raw, val)
	}
	return raw, nil
}

// NewRawAndTsFromJSON builds a raw value from a descriptor and the JSON object
// in data.  It works in two steps.  First, it constructs a slice of views onto
// the underlying JSON values.  This slice follows the order of the descriptor
// columns.  Second, it appends the descriptor ID and the values to a new
// buffer.
func NewRawAndTsFromJSON(d *Descriptor, tsCol int, data []byte) (zval.Encoding, nano.Ts, int, error) {
	var droppedFields int
	type jsonVal struct {
		val []byte
		typ jsonparser.ValueType
	}
	jsonVals := make([]jsonVal, 32) // Fixed size for stack allocation.
	if len(d.Type.Columns) > 32 {
		jsonVals = make([]jsonVal, len(d.Type.Columns))
	}
	n := 2 // Estimate for descriptor ID uvarint.
	callback := func(key []byte, val []byte, typ jsonparser.ValueType, offset int) error {
		if col, ok := d.ColumnOfField(string(key)); ok {
			jsonVals[col] = jsonVal{val, typ}
			n += len(val) + 1 // Estimate for zval and its length uvarint.
		} else {
			droppedFields++
		}
		return nil
	}
	if err := jsonparser.ObjectEach(data, callback); err != nil {
		return nil, 0, 0, err
	}
	raw := make([]byte, 0, n)
	var ts nano.Ts
	for i := range d.Type.Columns {
		val := jsonVals[i].val
		if i == tsCol {
			var err error
			ts, err = nano.Parse(val)
			if err != nil {
				ts, err = nano.ParseRFC3339Nano(val)
				if err != nil {
					return nil, 0, 0, err
				}
			}
		}
		switch jsonVals[i].typ {
		case jsonparser.Array:
			vals := make([][]byte, 0, 8) // Fixed size for stack allocation.
			callback := func(v []byte, typ jsonparser.ValueType, offset int, err error) {
				vals = append(vals, v)
			}
			if _, err := jsonparser.ArrayEach(val, callback); err != nil {
				return nil, 0, 0, err
			}
			raw = zval.AppendContainer(raw, vals)
			continue
		case jsonparser.Boolean:
			val = []byte{'F'}
			if val[0] == 't' {
				val = []byte{'T'}
			}
		case jsonparser.Null:
			val = nil
		case jsonparser.String:
			val = zeek.Unescape(val)
		}
		raw = zval.AppendValue(raw, val)
	}
	return raw, ts, droppedFields, nil
}

func NewRawAndTsFromZeekTSV(d *Descriptor, tsCol int, path []byte, data []byte) (zval.Encoding, nano.Ts, error) {
	vals := make([][]byte, 0, 32) // Fixed length for stack allocation.
	vals = append(vals, path)
	const separator = '\t'
	var start int
	for i, c := range data {
		if c == separator {
			vals = append(vals, data[start:i])
			start = i + 1
		}
	}
	vals = append(vals, data[start:])
	return NewRawAndTsFromZeekValues(d, tsCol, vals)
}

func NewRawAndTsFromZeekValues(d *Descriptor, tsCol int, vals [][]byte) (zval.Encoding, nano.Ts, error) {
	if nv, nc := len(vals), len(d.Type.Columns); nv != nc {
		// Don't pass vals to fmt.Errorf or it will escape to the heap.
		return nil, 0, fmt.Errorf("got %d values, expected %d", nv, nc)
	}
	n := 2 // Estimate for descriptor ID uvarint.
	for _, v := range vals {
		n += len(v) + 1 // Estimate for zval and its length uvarint.
	}
	raw := make([]byte, 0, n)
	var ts nano.Ts
	for i, val := range vals {
		var err error
		if i == tsCol {
			ts, err = nano.Parse(val)
			if err != nil {
				return nil, 0, err
			}
		}
		raw = appendZvalFromZeek(raw, d.Type.Columns[i].Type, val)
	}
	return raw, ts, nil
}

var (
	ErrUnterminated = errors.New("zson syntax error: unterminated container")
	ErrSyntax       = errors.New("zson syntax error")
)

type Parser struct {
	builder *zval.Builder
}

func NewParser() *Parser {
	return &Parser{
		builder: zval.NewBuilder(),
	}
}

func (p *Parser) Parse(desc *Descriptor, zson []byte) (zval.Encoding, error) {
	// XXX no validation on types from the descriptor, though we'll
	// want to add that to support eg the bytes type.
	// if we did this, we could also get at the ts field without
	// making a separate pass in the parser.
	builder := p.builder
	builder.Reset()
	n := len(zson)
	if len(zson) < 2 || zson[0] != '[' || zson[n-1] != ']' {
		return nil, ErrSyntax
	}
	zson = zson[1 : n-1]
	for len(zson) > 0 {
		rest, err := zsonParseField(builder, zson)
		if err != nil {
			return nil, err
		}
		zson = rest
	}
	return builder.Encode(), nil
}

const (
	semicolon    = byte(';')
	leftbracket  = byte('[')
	rightbracket = byte(']')
	backslash    = byte('\\')
)

// zsonParseContainer() parses the given byte array representing a container
// in the zson format.
// If there is no error, the first two return values are:
//  1. an array of zvals corresponding to the indivdiual elements
//  2. the passed-in byte array advanced past all the data that was parsed.
func zsonParseContainer(builder *zval.Builder, b []byte) ([]byte, error) {
	builder.Begin()
	// skip leftbracket
	b = b[1:]
	for {
		if len(b) == 0 {
			return nil, ErrUnterminated
		}
		if b[0] == rightbracket {
			builder.End()
			return b[1:], nil
		}
		rest, err := zsonParseField(builder, b)
		if err != nil {
			return nil, err
		}
		b = rest
	}
}

// zsonParseField() parses the given bye array representing any value
// in the zson format.
func zsonParseField(builder *zval.Builder, b []byte) ([]byte, error) {
	if b[0] == leftbracket {
		return zsonParseContainer(builder, b)
	}
	if len(b) >= 2 && b[0] == '-' && b[1] == ';' {
		builder.Append(nil)
		return b[2:], nil
	}
	to := 0
	from := 0
	for {
		if from >= len(b) {
			return nil, ErrUnterminated
		}
		switch b[from] {
		case semicolon:
			builder.Append(b[:to])
			return b[from+1:], nil
		case backslash:
			e, n := zeek.ParseEscape(b[from:])
			if n == 0 {
				panic("zeek.ParseEscape returned 0")
			}
			b[to] = e
			from += n
		default:
			b[to] = b[from]
			from++
		}
		to++
	}
}
