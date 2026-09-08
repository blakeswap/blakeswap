package storage

import (
	"context"
	"encoding"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"
)

// Traverse typed wallet maps and history slices incrementally. json.Encoder on
// an entire manifest first buffers the whole value, even with a streaming output
// Writer. Here only individual scalar/custom-JSON records use that encoder;
// archive RawMessages already own their bytes and write without another copy.
func writeJSONStream(ctx context.Context, w io.Writer, value any) error {
	return writeJSONValue(ctx, w, reflect.ValueOf(value), 0)
}
func jsonFields(v reflect.Value) map[string]reflect.Value {
	result := map[string]reflect.Value{}
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if f.PkgPath != "" && !f.Anonymous {
			continue
		}
		tag := strings.Split(f.Tag.Get("json"), ",")
		if tag[0] == "-" {
			continue
		}
		field := v.Field(i)
		if f.Anonymous && tag[0] == "" {
			inner := field
			if inner.Kind() == reflect.Pointer {
				if inner.IsNil() {
					continue
				}
				inner = inner.Elem()
			}
			if inner.Kind() == reflect.Struct {
				for k, x := range jsonFields(inner) {
					result[k] = x
				}
				continue
			}
		}
		omit := false
		for _, option := range tag[1:] {
			if option == "omitempty" {
				switch field.Kind() {
				case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
					omit = field.Len() == 0
				case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64, reflect.Interface, reflect.Pointer:
					omit = field.IsZero()
				}
			}
		}
		if omit {
			continue
		}
		name := tag[0]
		if name == "" {
			name = f.Name
		}
		result[name] = field
	}
	return result
}
func writeJSONValue(ctx context.Context, w io.Writer, v reflect.Value, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 128 {
		return errors.New("portable JSON nesting exceeds supported depth")
	}
	text := func(s string) error { _, err := io.WriteString(w, s); return err }
	if !v.IsValid() {
		return text("null")
	}
	if (v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer) && v.IsNil() {
		return text("null")
	}
	if v.CanInterface() {
		if raw, ok := v.Interface().(json.RawMessage); ok {
			if len(raw) == 0 {
				return text("null")
			}
			if !json.Valid(raw) {
				return errors.New("invalid portable JSON record")
			}
			_, err := w.Write(raw)
			return err
		}
		if m, ok := v.Interface().(json.Marshaler); ok {
			raw, err := m.MarshalJSON()
			if err != nil {
				return err
			}
			defer clear(raw)
			_, err = w.Write(raw)
			return err
		}
		if m, ok := v.Interface().(encoding.TextMarshaler); ok {
			raw, err := m.MarshalText()
			if err != nil {
				return err
			}
			encoded, err := json.Marshal(string(raw))
			if err != nil {
				return err
			}
			_, err = w.Write(encoded)
			return err
		}
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		return writeJSONValue(ctx, w, v.Elem(), depth+1)
	case reflect.Struct:
		fields := jsonFields(v)
		keys := make([]string, 0, len(fields))
		for name := range fields {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		if err := text("{"); err != nil {
			return err
		}
		for i, name := range keys {
			if i > 0 {
				if err := text(","); err != nil {
					return err
				}
			}
			key, _ := json.Marshal(name)
			if _, err := w.Write(key); err != nil {
				return err
			}
			if err := text(":"); err != nil {
				return err
			}
			if err := writeJSONValue(ctx, w, fields[name], depth+1); err != nil {
				return err
			}
		}
		return text("}")
	case reflect.Map:
		if v.IsNil() {
			return text("null")
		}
		if v.Type().Key().Kind() != reflect.String {
			return errors.New("portable map requires string identities")
		}
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		if err := text("{"); err != nil {
			return err
		}
		for i, key := range keys {
			if i > 0 {
				if err := text(","); err != nil {
					return err
				}
			}
			encoded, _ := json.Marshal(key.String())
			if _, err := w.Write(encoded); err != nil {
				return err
			}
			if err := text(":"); err != nil {
				return err
			}
			if err := writeJSONValue(ctx, w, v.MapIndex(key), depth+1); err != nil {
				return err
			}
		}
		return text("}")
	case reflect.Array, reflect.Slice:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return text("null")
		}
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
			break
		}
		if err := text("["); err != nil {
			return err
		}
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				if err := text(","); err != nil {
					return err
				}
			}
			if err := writeJSONValue(ctx, w, v.Index(i), depth+1); err != nil {
				return err
			}
		}
		return text("]")
	}
	raw, err := json.Marshal(v.Interface())
	if err != nil {
		return err
	}
	defer clear(raw)
	_, err = w.Write(raw)
	return err
}

func jsonDecodeFields(v reflect.Value) map[string]reflect.Value {
	result := map[string]reflect.Value{}
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if f.PkgPath != "" && !f.Anonymous {
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		field := v.Field(i)
		if f.Anonymous && name == "" {
			inner := field
			if inner.Kind() == reflect.Pointer {
				if inner.IsNil() {
					inner.Set(reflect.New(inner.Type().Elem()))
				}
				inner = inner.Elem()
			}
			if inner.Kind() == reflect.Struct {
				for key, x := range jsonDecodeFields(inner) {
					result[key] = x
				}
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		result[name] = field
	}
	return result
}
func readJSONStream(ctx context.Context, r io.Reader, out any) error {
	v := reflect.ValueOf(out)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return errors.New("portable output must be a pointer")
	}
	next := reflect.New(v.Elem().Type()).Elem()
	decoder := json.NewDecoder(r)
	decoder.UseNumber()
	if err := readJSONValue(ctx, decoder, next, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("trailing portable JSON value")
		}
		return err
	}
	v.Elem().Set(next)
	return nil
}
func readJSONValue(ctx context.Context, d *json.Decoder, v reflect.Value, depth int) error {
	return readJSONToken(ctx, d, v, depth, nil, false)
}
func readJSONToken(ctx context.Context, d *json.Decoder, v reflect.Value, depth int, token any, consumed bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 128 {
		return errors.New("portable JSON nesting exceeds supported depth")
	}
	if v.Kind() == reflect.Pointer {
		custom := v.Type().Implements(reflect.TypeFor[json.Unmarshaler]()) || v.Type().Implements(reflect.TypeFor[encoding.TextUnmarshaler]())
		if custom && !consumed {
			return d.Decode(v.Addr().Interface())
		}
		if !consumed {
			var err error
			token, err = d.Token()
			if err != nil {
				return err
			}
		}
		if token == nil {
			v.SetZero()
			return nil
		}
		v.Set(reflect.New(v.Type().Elem()))
		return readJSONToken(ctx, d, v.Elem(), depth+1, token, true)
	}
	if v.CanAddr() {
		if _, ok := v.Addr().Interface().(json.Unmarshaler); ok {
			return d.Decode(v.Addr().Interface())
		}
		if _, ok := v.Addr().Interface().(encoding.TextUnmarshaler); ok {
			return d.Decode(v.Addr().Interface())
		}
	}
	switch v.Kind() {
	case reflect.Map, reflect.Struct, reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
			return d.Decode(v.Addr().Interface())
		}
		if !consumed {
			var err error
			token, err = d.Token()
			if err != nil {
				return err
			}
		}
		if token == nil {
			v.SetZero()
			return nil
		}
		object := v.Kind() == reflect.Map || v.Kind() == reflect.Struct
		opening, closing := json.Delim('['), json.Delim(']')
		if object {
			opening, closing = '{', '}'
		}
		if token != opening {
			return errors.New("portable JSON shape does not match its typed field")
		}
		var fields map[string]reflect.Value
		if v.Kind() == reflect.Slice {
			v.Set(reflect.MakeSlice(v.Type(), 0, 0))
		}
		if v.Kind() == reflect.Map {
			if v.Type().Key().Kind() != reflect.String {
				return errors.New("portable map requires string identities")
			}
			v.Set(reflect.MakeMap(v.Type()))
		}
		if v.Kind() == reflect.Struct {
			fields = jsonDecodeFields(v)
		}
		seen := map[string]bool{}
		index := 0
		for d.More() {
			if object {
				token, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := token.(string)
				if !ok || seen[key] {
					return errors.New("invalid or repeated portable JSON field")
				}
				seen[key] = true
				if v.Kind() == reflect.Struct {
					field, ok := fields[key]
					if !ok {
						return errors.New("unsupported portable JSON field")
					}
					if err := readJSONValue(ctx, d, field, depth+1); err != nil {
						return err
					}
				} else {
					entry := reflect.New(v.Type().Elem()).Elem()
					if err := readJSONValue(ctx, d, entry, depth+1); err != nil {
						return err
					}
					mapKey := reflect.New(v.Type().Key()).Elem()
					mapKey.SetString(key)
					v.SetMapIndex(mapKey, entry)
				}
			} else {
				if v.Kind() == reflect.Array {
					if index >= v.Len() {
						return errors.New("portable array exceeds its typed bound")
					}
					if err := readJSONValue(ctx, d, v.Index(index), depth+1); err != nil {
						return err
					}
				} else {
					entry := reflect.New(v.Type().Elem()).Elem()
					if err := readJSONValue(ctx, d, entry, depth+1); err != nil {
						return err
					}
					v.Set(reflect.Append(v, entry))
				}
				index++
			}
		}
		end, err := d.Token()
		if err != nil {
			return err
		}
		if end != closing {
			return errors.New("incomplete portable JSON container")
		}
		return nil
	default:
		if consumed {
			raw, err := json.Marshal(token)
			if err != nil {
				return err
			}
			return json.Unmarshal(raw, v.Addr().Interface())
		}
		return d.Decode(v.Addr().Interface())
	}
}

// JSONRecordDecoder consumes one typed value at a time from an authenticated
// stream. End rejects any missing, additional or incomplete top-level record.
type JSONRecordDecoder struct{ decoder *json.Decoder }

func NewJSONRecordDecoder(r io.Reader) *JSONRecordDecoder {
	d := json.NewDecoder(r)
	d.UseNumber()
	return &JSONRecordDecoder{decoder: d}
}
func (d *JSONRecordDecoder) Decode(ctx context.Context, out any) error {
	v := reflect.ValueOf(out)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return errors.New("portable output must be a pointer")
	}
	next := reflect.New(v.Elem().Type()).Elem()
	if err := readJSONValue(ctx, d.decoder, next, 0); err != nil {
		return err
	}
	v.Elem().Set(next)
	return nil
}
func (d *JSONRecordDecoder) End() error {
	if _, err := d.decoder.Token(); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("unexpected extra portable record")
	}
	return nil
}
func WriteJSONRecord(ctx context.Context, w io.Writer, value any) error {
	if err := writeJSONStream(ctx, w, value); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}
