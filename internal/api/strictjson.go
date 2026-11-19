package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// decodeStrict decodes one JSON value into v and refuses fields that v does
// not have, at any depth, like a json.Decoder with DisallowUnknownFields.
// Unlike a Decoder it never copies the input into a buffer of its own: a
// 20 MB send request stays one 20 MB buffer.
func decodeStrict(data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		return err
	}
	return checkFields(data, reflect.TypeOf(v))
}

// jsonSpan keeps a JSON value as it appears in the request, without copying
// it (json.Unmarshal hands Unmarshalers a slice of its input). Use it only
// with decodeStrict or json.Unmarshal on a body that stays unchanged.
type jsonSpan []byte

func (s *jsonSpan) UnmarshalJSON(b []byte) error {
	*s = b
	return nil
}

// isNull reports whether the value is absent or JSON null.
func (s jsonSpan) isNull() bool { return len(s) == 0 || string(s) == "null" }

var unmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// checkFields walks a JSON value already decoded into a value of type t,
// and returns json's "unknown field" error for the first object key (in
// sorted order) that t has no field for.
func checkFields(data []byte, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Implements(unmarshalerType) || reflect.PointerTo(t).Implements(unmarshalerType) {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		fields := structFields(t)
		var unknown []string
		var err error
		eachMember(data, func(key string, val []byte) bool {
			ft, ok := fields.lookup(key)
			if !ok {
				unknown = append(unknown, key)
				return true
			}
			err = checkFields(val, ft)
			return err == nil
		})
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return fmt.Errorf("json: unknown field %q", unknown[0])
		}
		return err
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return nil // []byte is a base64 string
		}
		var err error
		eachElement(data, func(val []byte) bool {
			err = checkFields(val, t.Elem())
			return err == nil
		})
		return err
	case reflect.Map:
		var err error
		eachMember(data, func(_ string, val []byte) bool {
			err = checkFields(val, t.Elem())
			return err == nil
		})
		return err
	}
	return nil
}

// fieldSet maps JSON names to field types, matched like encoding/json:
// exactly first, then ignoring case.
type fieldSet map[string]reflect.Type

func (f fieldSet) lookup(key string) (reflect.Type, bool) {
	if t, ok := f[key]; ok {
		return t, true
	}
	for name, t := range f {
		if strings.EqualFold(name, key) {
			return t, true
		}
	}
	return nil, false
}

var fieldCache sync.Map // reflect.Type -> fieldSet

func structFields(t reflect.Type) fieldSet {
	if f, ok := fieldCache.Load(t); ok {
		return f.(fieldSet)
	}
	f := fieldSet{}
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if sf.Anonymous && name == "" {
			et := sf.Type
			if et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				for k, v := range structFields(et) {
					if _, dup := f[k]; !dup {
						f[k] = v
					}
				}
				continue
			}
		}
		if !sf.IsExported() {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		f[name] = sf.Type
	}
	fieldCache.Store(t, f)
	return f
}

// The walkers below expect JSON that json.Unmarshal has already accepted,
// and stop quietly at anything else.

func skipSpace(d []byte, i int) int {
	for i < len(d) && (d[i] == ' ' || d[i] == '\t' || d[i] == '\n' || d[i] == '\r') {
		i++
	}
	return i
}

// skipString returns the index after the string that starts at d[i].
func skipString(d []byte, i int) int {
	for i++; i < len(d); {
		j := bytes.IndexAny(d[i:], `"\`)
		if j < 0 {
			return len(d)
		}
		i += j
		if d[i] == '"' {
			return i + 1
		}
		i += 2 // an escape
	}
	return len(d)
}

// skipValue returns the index after the value that starts at d[i].
func skipValue(d []byte, i int) int {
	if i >= len(d) {
		return i
	}
	switch d[i] {
	case '"':
		return skipString(d, i)
	case '{', '[':
		depth := 0
		for i < len(d) {
			switch d[i] {
			case '"':
				i = skipString(d, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return i
	}
	for i < len(d) && !strings.ContainsRune(",}] \t\r\n", rune(d[i])) {
		i++
	}
	return i
}

// eachMember calls fn with every key and value of the object in d, until fn
// returns false. Anything but an object (null included) has no members.
func eachMember(d []byte, fn func(key string, val []byte) bool) {
	i := skipSpace(d, 0)
	if i >= len(d) || d[i] != '{' {
		return
	}
	i++
	for {
		i = skipSpace(d, i)
		if i >= len(d) || d[i] != '"' {
			return
		}
		ks := i
		i = skipString(d, i)
		raw := d[ks:i]
		var key string
		if bytes.IndexByte(raw, '\\') < 0 {
			key = string(raw[1 : len(raw)-1])
		} else if json.Unmarshal(raw, &key) != nil {
			return
		}
		i = skipSpace(d, i)
		if i >= len(d) || d[i] != ':' {
			return
		}
		vs := skipSpace(d, i+1)
		i = skipValue(d, vs)
		if !fn(key, d[vs:i]) {
			return
		}
		i = skipSpace(d, i)
		if i >= len(d) || d[i] != ',' {
			return
		}
		i++
	}
}

// eachElement calls fn with every element of the array in d, until fn
// returns false.
func eachElement(d []byte, fn func(val []byte) bool) {
	i := skipSpace(d, 0)
	if i >= len(d) || d[i] != '[' {
		return
	}
	i++
	for {
		vs := skipSpace(d, i)
		if vs >= len(d) || d[vs] == ']' {
			return
		}
		i = skipValue(d, vs)
		if !fn(d[vs:i]) {
			return
		}
		i = skipSpace(d, i)
		if i >= len(d) || d[i] != ',' {
			return
		}
		i++
	}
}
