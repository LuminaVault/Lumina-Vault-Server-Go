package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"
)

// Content types exactly as Hummingbird sends them (lowercase charset; Echo's
// own helpers say UTF-8, which parity flags).
const (
	ContentTypeJSON = "application/json; charset=utf-8"
	ContentTypeText = "text/plain; charset=utf-8"
)

// Text writes s as Hummingbird writes a String response.
func Text(c *echo.Context, status int, s string) error {
	return c.Blob(status, ContentTypeText, []byte(s))
}

// JSON writes v with the Swift server's exact Content-Type.
func JSON(c *echo.Context, status int, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Blob(status, ContentTypeJSON, b)
}

// maxBody mirrors Hummingbird's default request body limit (2 MiB).
const maxBody = 2 << 20

// Bind decodes the request body into *T as Foundation's JSONDecoder does, and
// turns failures into the 400 messages Hummingbird returns. The strings are
// verbatim from testdata/golden/errors-http.json. Struct fields without
// `omitempty` are required; unknown keys are ignored.
func Bind[T any](c *echo.Context) (*T, error) {
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, maxBody))
	if err != nil {
		return nil, err
	}
	var out T
	if err := decode(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func invalid(detail string) *Error {
	return NewError(http.StatusBadRequest, "The given data was not valid input: "+detail)
}

var notJSON = invalid("The given data was not valid JSON.")

func decode(raw []byte, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return notJSON
	}
	rv := reflect.ValueOf(dst).Elem()
	if rv.Kind() != reflect.Struct {
		if err := json.Unmarshal(trimmed, dst); err != nil {
			return mapErr("", err)
		}
		return nil
	}
	return decodeStruct(nil, trimmed, rv)
}

// decodeStruct decodes a JSON object into rv field by field, in declaration
// order (Foundation's synthesized init(from:) order, so the first failing key
// is the one Swift reports). path is the coding path of the object itself.
//
// Hummingbird formats nested failures as Request.decode does: a missing key
// names only the key ("Coding key `completed` not found."), while null and
// type mismatches name the whole dotted path with array indices
// ("Type mismatch for `reminders.0.title` key, ...").
func decodeStruct(path []string, raw []byte, rv reflect.Value) error {
	if len(raw) == 0 || raw[0] != '{' {
		return mismatch(strings.Join(path, "."), "Dictionary<String, Any>")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return notJSON
	}
	rt := rv.Type()
	for i := range rt.NumField() {
		f := rt.Field(i)
		name, optional := jsonName(f)
		if name == "" {
			continue
		}
		val, present := obj[name]
		val = bytes.TrimSpace(val)
		isNull := present && bytes.Equal(val, []byte("null"))
		required := !optional && f.Type.Kind() != reflect.Pointer
		keyPath := append(append([]string{}, path...), name)
		switch {
		case !present && required:
			return NewError(http.StatusBadRequest, fmt.Sprintf("Coding key `%s` not found.", name))
		case isNull && required:
			return nullErr(keyPath, f.Type)
		case !present || isNull:
			continue
		}
		if err := decodeValue(keyPath, val, rv.Field(i)); err != nil {
			return err
		}
	}
	return nil
}

var unmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// nullErr is Foundation's error for null where a non-optional value is
// required. swift-foundation checks for null (valueNotFound) before decoding
// strings, numbers, containers and Decodable types, but decodes Bool with a
// bare `case .bool` match, so a null Bool is a type mismatch (confirmed by
// parity CI against the Swift image).
func nullErr(path []string, t reflect.Type) *Error {
	key := strings.Join(path, ".")
	if t.Kind() == reflect.Bool {
		return mismatch(key, "Bool")
	}
	return NewError(http.StatusBadRequest, fmt.Sprintf("Value not found for `%s` key.", key))
}

// decodeValue decodes raw (not null unless fv is a pointer) into fv, recursing
// into structs, slices and string-keyed maps so errors carry the full path.
func decodeValue(path []string, raw []byte, fv reflect.Value) error {
	t := fv.Type()
	if t.Kind() == reflect.Pointer {
		if bytes.Equal(raw, []byte("null")) {
			return nil
		}
		elem := reflect.New(t.Elem())
		if err := decodeValue(path, raw, elem.Elem()); err != nil {
			return err
		}
		fv.Set(elem)
		return nil
	}
	key := strings.Join(path, ".")
	if reflect.PointerTo(t).Implements(unmarshalerType) {
		if err := json.Unmarshal(raw, fv.Addr().Interface()); err != nil {
			return mapErr(key, err)
		}
		return nil
	}
	switch {
	case t.Kind() == reflect.Struct:
		return decodeStruct(path, raw, fv)
	case t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8:
		if len(raw) == 0 || raw[0] != '[' {
			return mismatch(key, "Array<Any>")
		}
		var elems []json.RawMessage
		if err := json.Unmarshal(raw, &elems); err != nil {
			return notJSON
		}
		out := reflect.MakeSlice(t, len(elems), len(elems))
		for i, e := range elems {
			e = bytes.TrimSpace(e)
			elemPath := append(append([]string{}, path...), strconv.Itoa(i))
			if bytes.Equal(e, []byte("null")) && t.Elem().Kind() != reflect.Pointer {
				return nullErr(elemPath, t.Elem())
			}
			if err := decodeValue(elemPath, e, out.Index(i)); err != nil {
				return err
			}
		}
		fv.Set(out)
		return nil
	case t.Kind() == reflect.Map && t.Key().Kind() == reflect.String:
		if len(raw) == 0 || raw[0] != '{' {
			return mismatch(key, "Dictionary<String, Any>")
		}
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			return notJSON
		}
		keys := make([]string, 0, len(entries))
		for k := range entries {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := reflect.MakeMapWithSize(t, len(entries))
		for _, k := range keys {
			e := bytes.TrimSpace(entries[k])
			elemPath := append(append([]string{}, path...), k)
			if bytes.Equal(e, []byte("null")) && t.Elem().Kind() != reflect.Pointer {
				return nullErr(elemPath, t.Elem())
			}
			v := reflect.New(t.Elem()).Elem()
			if err := decodeValue(elemPath, e, v); err != nil {
				return err
			}
			out.SetMapIndex(reflect.ValueOf(k).Convert(t.Key()), v)
		}
		fv.Set(out)
		return nil
	}
	if err := json.Unmarshal(raw, fv.Addr().Interface()); err != nil {
		return mapErr(key, err)
	}
	return nil
}

func mapErr(key string, err error) error {
	var corrupted *CorruptedError
	if errors.As(err, &corrupted) {
		return invalid(corrupted.Message)
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return mismatch(key, swiftType(typeErr.Type))
	}
	return notJSON
}

func mismatch(key, typ string) *Error {
	return NewError(http.StatusBadRequest, fmt.Sprintf("Type mismatch for `%s` key, expected `%s` type.", key, typ))
}

// swiftType names a Go type as Foundation's DecodingError does.
func swiftType(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "String"
	case reflect.Bool:
		return "Bool"
	case reflect.Int, reflect.Int64, reflect.Int32, reflect.Int16, reflect.Int8,
		reflect.Uint, reflect.Uint64, reflect.Uint32, reflect.Uint16, reflect.Uint8:
		return "Int"
	case reflect.Float64, reflect.Float32:
		return "Double"
	case reflect.Slice, reflect.Array:
		return "Array<Any>"
	default:
		return "Dictionary<String, Any>"
	}
}

func jsonName(f reflect.StructField) (name string, optional bool) {
	if !f.IsExported() {
		return "", false
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	name, opts, _ := strings.Cut(tag, ",")
	if name == "" {
		name = f.Name
	}
	return name, strings.Contains(opts, "omitempty")
}
