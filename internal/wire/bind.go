package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
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
	var obj map[string]json.RawMessage
	if trimmed[0] != '{' {
		return mismatch("", "Dictionary<String, Any>")
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
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
		isNull := present && bytes.Equal(bytes.TrimSpace(val), []byte("null"))
		required := !optional && f.Type.Kind() != reflect.Pointer
		switch {
		case !present && required:
			return NewError(http.StatusBadRequest, fmt.Sprintf("Coding key `%s` not found.", name))
		case isNull && required:
			return NewError(http.StatusBadRequest, fmt.Sprintf("Value not found for `%s` key.", name))
		case !present || isNull:
			continue
		}
		if err := json.Unmarshal(val, rv.Field(i).Addr().Interface()); err != nil {
			return mapErr(name, err)
		}
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
