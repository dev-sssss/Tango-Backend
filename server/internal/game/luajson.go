package game

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The original game code built its wire messages as Lua tables and encoded
// them with Nakama's converter. These helpers reproduce that encoding exactly
// so existing clients see identical JSON:
//
//   - a table with no positive integer keys encodes as an object ({} when empty);
//   - a table whose keys are positive integers encodes as an array up to the
//     largest key, with null for missing entries;
//   - a field set to nil is absent;
//   - whole numbers encode without a fractional part.

// Obj builds a JSON object from key/value pairs, skipping nil values (a Lua
// field assigned nil does not exist). Typed nil pointers are skipped too.
func Obj(kv ...any) map[string]any {
	m := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		k := kv[i].(string)
		if isNil(kv[i+1]) {
			continue
		}
		m[k] = kv[i+1]
	}
	return m
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface:
		if rv.IsNil() {
			// A nil map/slice is an empty Lua table, not a missing field,
			// except for pointers and interfaces.
			return rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface
		}
	}
	return false
}

// Opt dereferences an optional value, returning nil when unset.
func Opt[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// SeatTable encodes an integer-keyed Lua table (seat -> value).
func SeatTable[T any](m map[int]T) any {
	maxN := 0
	for k := range m {
		if k > maxN {
			maxN = k
		}
	}
	if maxN == 0 {
		return map[string]any{}
	}
	arr := make([]any, maxN)
	for k, v := range m {
		if k >= 1 {
			arr[k-1] = v
		}
	}
	return arr
}

// List encodes a Lua sequence: an empty sequence is an empty object.
func List[T any](xs []T) any {
	if len(xs) == 0 {
		return map[string]any{}
	}
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// Normalize converts decoded JSON (or values built from it) into the shape the
// Lua runtime would re-encode: nulls disappear from objects, arrays drop
// trailing nulls and empty arrays become empty objects. Numbers are kept.
func Normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			if x == nil {
				continue
			}
			out[k] = Normalize(x)
		}
		return out
	case []any:
		n := len(t)
		for n > 0 && t[n-1] == nil {
			n--
		}
		if n == 0 {
			return map[string]any{}
		}
		out := make([]any, n)
		for i := 0; i < n; i++ {
			out[i] = Normalize(t[i])
		}
		return out
	default:
		return v
	}
}

// Encode marshals like nk.json_encode. Float values that are whole numbers
// are written as integers (Lua numbers are converted to int64 when whole).
func Encode(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	_ = enc.Encode(intify(v))
	return strings.TrimSuffix(buf.String(), "\n")
}

func EncodeBytes(v any) []byte { return []byte(Encode(v)) }

func intify(v any) any {
	switch t := v.(type) {
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) && t >= -9.2e18 && t <= 9.2e18 {
			return int64(t)
		}
		return t
	case float32:
		return intify(float64(t))
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = intify(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = intify(x)
		}
		return out
	default:
		return v
	}
}

// Decode parses JSON like nk.json_decode (numbers become float64).
func Decode(s string) (any, error) {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, err
	}
	return v, nil
}

// DecodeObject decodes a JSON object; ok is false for invalid JSON or a
// non-object (Lua: type(decoded) ~= "table" for scalars; arrays are tables
// too, so arrays return an empty map with ok=true).
func DecodeObject(s string) (map[string]any, bool) {
	v, err := Decode(s)
	if err != nil {
		return nil, false
	}
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case []any:
		m := make(map[string]any, len(t))
		for i, x := range t {
			if x != nil {
				m[strconv.Itoa(i+1)] = x
			}
		}
		return m, true
	}
	return nil, false
}

// ToNumber implements Lua's tonumber for the value types JSON produces.
func ToNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
			n, err := strconv.ParseUint(s[2:], 16, 64)
			if err != nil {
				return 0, false
			}
			return float64(n), true
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(f) && s != "nan" {
			return 0, false
		}
		if strings.ContainsAny(s, "nN") && !strings.ContainsAny(s, "eE") {
			// Lua does not accept "nan"/"inf" words.
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// Truthy implements Lua truthiness for decoded JSON values.
func Truthy(v any) bool {
	if v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return true
}

// Or returns a when it is Lua-truthy, else b.
func Or(a, b any) any {
	if Truthy(a) {
		return a
	}
	return b
}

// Str returns v when it is a string.
func Str(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// SortedIntKeys returns map keys in ascending order (Lua array-part order).
func SortedIntKeys[T any](m map[int]T) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// Clone deep-copies decoded JSON values.
func Clone(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = Clone(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = Clone(x)
		}
		return out
	default:
		return v
	}
}

// Map returns v as an object, or an empty object.
func Map(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}$`)

// ValidUUID reports whether s is a UUID in canonical or compact form.
func ValidUUID(s string) bool { return uuidRe.MatchString(s) }
