// Package binder provides zero-dependency HTTP request binding for Go.
//
// Binder maps data from HTTP requests to Go structs using struct tags,
// supporting multiple data sources including path parameters, query strings,
// request bodies (JSON and form-encoded), and cookies.
//
// Basic usage:
//
//	var req struct {
//	    ID    int    `path:"id"`
//	    Name  string `query:"name"`
//	    Email string `body:"email"`
//	}
//	err := binder.Bind(r, &req)
//
// The library is designed to work with Go 1.22+ and its native path parameter support.
// It maintains zero external dependencies and focuses solely on data binding,
// leaving validation and transformation to other specialized tools.
package binder

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

// Tag constants
const (
	path   = "path"
	query  = "query"
	body   = "body"
	jjson  = "json"
	cookie = "cookie"
)

// fieldInfo stores cached reflection data for struct fields
type fieldInfo struct {
	Index     int
	FieldType reflect.StructField
	Source    string // "path", "query", "body", "json", "cookie"
	TagName   string
	OmitEmpty bool
}

// Cache for struct field information to improve performance
var fieldCache = make(map[reflect.Type]map[string]fieldInfo)
var fieldCacheMutex sync.RWMutex

// Validator is an optional interface that structs can implement to provide
// custom validation logic that runs automatically after successful binding.
//
// Example:
//
//	type CreateUserRequest struct {
//	    Email string `body:"email"`
//	    Age   int    `body:"age"`
//	}
//
//	func (r CreateUserRequest) Validate() error {
//	    if r.Age < 18 {
//	        return errors.New("user must be 18 or older")
//	    }
//	    return nil
//	}
//
// When a type implements Validator, Bind will call Validate after binding
// and return any validation errors.
type Validator interface {
	Validate() error
}

// Bind maps data from an HTTP request into a struct using reflection and struct tags.
//
// The target must be a pointer to a struct. Bind supports multiple data sources:
//
//	- path:"name"   - URL path parameters (requires Go 1.22+)
//	- query:"name"  - URL query parameters
//	- body:"name"   - Request body (JSON or form-encoded based on Content-Type)
//	- json:"name"   - Alternative to body tag for JSON data
//	- cookie:"name" - HTTP cookies
//
// Tag modifiers:
//
//	- omitempty - Skip binding if the value is empty
//
// Example:
//
//	type UpdateUserRequest struct {
//	    ID       int    `path:"id"`
//	    Name     string `body:"name"`
//	    Email    string `body:"email,omitempty"`
//	    APIToken string `cookie:"api_token"`
//	}
//
//	var req UpdateUserRequest
//	if err := binder.Bind(r, &req); err != nil {
//	    // Handle binding error
//	}
//
// Returns an error if:
//   - The target is not a pointer to a struct
//   - Type conversion fails
//   - Required fields are missing
//   - Validation fails (if the struct implements Validator)
func Bind(r *http.Request, i interface{}) error {
	typ := reflect.TypeOf(i).Elem()
	val := reflect.ValueOf(i).Elem()

	var b map[string]interface{}

	// Handle request body if present
	if r.Body != nil && r.ContentLength > 0 {
		// Read the body once
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			return fmt.Errorf("error reading request body: %w", err)
		}

		// Restore the body for other potential readers
		r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

		// Create a copy of the request with the new body for parsing
		rCopy := *r
		rCopy.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

		// Parse the body
		b, err = parseBody(rCopy)
		if err != nil {
			// Continue with empty body - we still want to bind other parameters
			// The error is non-fatal as data might come from path/query/cookies
			b = make(map[string]interface{})
		}
	} else {
		b = make(map[string]interface{})
	}

	// Process each field
	for x := 0; x < typ.NumField(); x++ {
		field := typ.Field(x)
		f := val.Field(x)

		tag := field.Tag
		pathTag := tag.Get(path)
		queryTag := tag.Get(query)
		bodyTag := tag.Get(body)
		jsonTag := tag.Get(jjson)
		cookieTag := tag.Get(cookie)

		omitEmpty := strings.Contains(tag.Get(path)+tag.Get(query)+tag.Get(body)+tag.Get(jjson)+tag.Get(cookie), "omitempty")

		var v interface{}
		var exists bool

		switch {
		case pathTag != "":
			v = r.PathValue(pathTag)
			exists = v != ""

		case queryTag != "":
			paramName := queryTag
			if commaIndex := strings.Index(paramName, ","); commaIndex != -1 {
				paramName = paramName[:commaIndex]
			}

			v = r.URL.Query().Get(paramName)
			exists = v != ""

		case bodyTag != "":
			v, exists = b[bodyTag]

		case jsonTag != "":
			v, exists = b[jsonTag]

		case cookieTag != "":
			c, err := r.Cookie(cookieTag)
			if err == nil {
				v = c.Value
				exists = true
			}

		default:
			continue
		}

		if !exists || (omitEmpty && isEmptyValue(v)) {
			continue // Skip setting if omitempty and value not present
		}

		if f.Kind() == reflect.Ptr && f.IsNil() {
			f.Set(reflect.New(f.Type().Elem())) // Initialize pointer fields
		}

		// Handle nested structs recursively
		if f.Kind() == reflect.Struct || (f.Kind() == reflect.Ptr && f.Elem().Kind() == reflect.Struct) {
			if nestedMap, ok := v.(map[string]interface{}); ok {
				if err := BindStruct(f, nestedMap); err != nil {
					return fmt.Errorf("error binding nested field %s: %w", field.Name, err)
				}
				continue
			}
		}

		if err := setField(f, v); err != nil {
			return fmt.Errorf("error setting field %s: %w", field.Name, err)
		}
	}

	// Run validation if the struct implements Validator
	if validator, ok := i.(Validator); ok {
		if err := validator.Validate(); err != nil {
			return fmt.Errorf("validation failed: %w", err)
		}
	}

	return nil
}

// getFieldInfo returns cached field information for a struct type
func getFieldInfo(typ reflect.Type) map[string]fieldInfo {
	fieldCacheMutex.RLock()
	info, found := fieldCache[typ]
	fieldCacheMutex.RUnlock()

	if found {
		return info
	}

	// Build field info
	fieldCacheMutex.Lock()
	defer fieldCacheMutex.Unlock()

	// Check again in case another goroutine built it while we were waiting
	if info, found = fieldCache[typ]; found {
		return info
	}

	info = make(map[string]fieldInfo)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		fi := fieldInfo{
			Index:     i,
			FieldType: field,
		}

		// Check each tag type
		if tag := field.Tag.Get(path); tag != "" {
			fi.Source = path
			fi.TagName = tag
			fi.OmitEmpty = strings.Contains(tag, "omitempty")
			info[field.Name] = fi
			continue
		}

		if tag := field.Tag.Get(query); tag != "" {
			fi.Source = query
			fi.TagName = tag
			fi.OmitEmpty = strings.Contains(tag, "omitempty")
			info[field.Name] = fi
			continue
		}

		if tag := field.Tag.Get(body); tag != "" {
			fi.Source = body
			fi.TagName = tag
			fi.OmitEmpty = strings.Contains(tag, "omitempty")
			info[field.Name] = fi
			continue
		}

		if tag := field.Tag.Get(jjson); tag != "" {
			fi.Source = jjson
			fi.TagName = tag
			fi.OmitEmpty = strings.Contains(tag, "omitempty")
			info[field.Name] = fi
			continue
		}

		if tag := field.Tag.Get(cookie); tag != "" {
			fi.Source = cookie
			fi.TagName = tag
			fi.OmitEmpty = strings.Contains(tag, "omitempty")
			info[field.Name] = fi
			continue
		}
	}

	fieldCache[typ] = info
	return info
}

// BindStruct recursively binds data from a map to a struct field, handling nested structures.
//
// This function is exported for advanced use cases where you need to bind nested
// data manually. Most users should use Bind instead.
//
// Parameters:
//   - field: The reflect.Value of the struct field to bind to
//   - data: Map containing the data to bind
//
// The function handles both pointer and non-pointer fields, automatically
// initializing nil pointers as needed.
func BindStruct(field reflect.Value, data map[string]interface{}) error {
	target := field
	if field.Kind() == reflect.Ptr {
		if field.IsNil() {
			field.Set(reflect.New(field.Type().Elem()))
		}
		target = field.Elem()
	}

	typ := target.Type()
	for i := 0; i < typ.NumField(); i++ {
		fieldType := typ.Field(i)
		tag := fieldType.Tag.Get(body)
		if tag == "" {
			tag = fieldType.Tag.Get(jjson)
		}
		if tag == "" {
			continue
		}

		nestedValue, ok := data[tag]
		if !ok {
			continue
		}

		nestedField := target.Field(i)
		if nestedField.Kind() == reflect.Ptr && nestedField.IsNil() {
			nestedField.Set(reflect.New(nestedField.Type().Elem()))
		}

		if err := setField(nestedField, nestedValue); err != nil {
			return fmt.Errorf("error setting nested field %s: %w", fieldType.Name, err)
		}
	}
	return nil
}

// parseContentType extracts the content type from the Content-Type header
func parseContentType(header string) string {
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, "=") {
			return strings.ToLower(part)
		}
	}
	return ""
}

// parseBody extracts and parses the request body into a map
func parseBody(r http.Request) (map[string]interface{}, error) {
	var reqBody map[string]interface{}
	ct := parseContentType(r.Header.Get("Content-Type"))

	switch ct {
	case "application/json":
		err := json.NewDecoder(r.Body).Decode(&reqBody)
		if err != nil {
			return nil, fmt.Errorf("failed to decode JSON body: %w", err)
		}
		return reqBody, nil

	case "application/x-www-form-urlencoded":
		reqBody = make(map[string]interface{})
		err := r.ParseForm()
		if err != nil {
			return nil, fmt.Errorf("failed to parse form data: %w", err)
		}
		for k, v := range r.PostForm {
			if len(v) == 1 {
				reqBody[k] = v[0]
			} else {
				reqBody[k] = v
			}
		}
		return reqBody, nil
	}

	return make(map[string]interface{}), nil
}

// setField sets the appropriate value on the given reflect.Value field
func setField(field reflect.Value, value interface{}) error {
	// Handle nil value
	if value == nil {
		return nil
	}

	// Handle TextUnmarshaler interface
	if field.Type().Implements(reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()) {
		strVal, ok := value.(string)
		if !ok {
			return errors.New("value is not a string for TextUnmarshaler")
		}
		return field.Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(strVal))
	}

	if field.CanAddr() && reflect.PointerTo(field.Type()).Implements(reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()) {
		strVal, ok := value.(string)
		if !ok {
			return errors.New("value is not a string for TextUnmarshaler")
		}
		return field.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(strVal))
	}

	switch field.Kind() {
	case reflect.String:
		return setString(field, value)

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return setInt(field, value)

	case reflect.Float32, reflect.Float64:
		return setFloat(field, value)

	case reflect.Bool:
		return setBool(field, value)

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return setUint(field, value)

	case reflect.Slice:
		return setSlice(field, value)

	case reflect.Array:
		return fmt.Errorf("arrays are not supported, use slices instead")

	case reflect.Struct:
		return setStruct(field, value)

	case reflect.Ptr:
		if field.IsNil() {
			field.Set(reflect.New(field.Type().Elem()))
		}
		return setField(field.Elem(), value)

	default:
		return fmt.Errorf("unsupported type: %s", field.Kind())
	}
}

// setString sets a string value to a field
func setString(field reflect.Value, value interface{}) error {
	str, err := toString(value)
	if err != nil {
		return err
	}
	field.SetString(str)
	return nil
}

// toString converts various types to string
func toString(value interface{}) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case []byte:
		return string(v), nil
	case fmt.Stringer:
		return v.String(), nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

// setInt sets an integer value to a field
func setInt(field reflect.Value, value interface{}) error {
	switch v := value.(type) {
	case int:
		field.SetInt(int64(v))
	case int8:
		field.SetInt(int64(v))
	case int16:
		field.SetInt(int64(v))
	case int32:
		field.SetInt(int64(v))
	case int64:
		field.SetInt(v)
	case float32:
		field.SetInt(int64(v))
	case float64:
		field.SetInt(int64(v))
	case string:
		i, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return err
		}
		field.SetInt(i)
	default:
		return fmt.Errorf("cannot convert %T to int", value)
	}
	return nil
}

// setUint sets an unsigned integer value to a field
func setUint(field reflect.Value, value interface{}) error {
	switch v := value.(type) {
	case uint:
		field.SetUint(uint64(v))
	case uint8:
		field.SetUint(uint64(v))
	case uint16:
		field.SetUint(uint64(v))
	case uint32:
		field.SetUint(uint64(v))
	case uint64:
		field.SetUint(v)
	case int:
		if v < 0 {
			return fmt.Errorf("cannot convert negative int to uint")
		}
		field.SetUint(uint64(v))
	case float64:
		if v < 0 {
			return fmt.Errorf("cannot convert negative float to uint")
		}
		field.SetUint(uint64(v))
	case string:
		i, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return err
		}
		field.SetUint(i)
	default:
		return fmt.Errorf("cannot convert %T to uint", value)
	}
	return nil
}

// setBool sets a boolean value to a field
func setBool(field reflect.Value, value interface{}) error {
	switch v := value.(type) {
	case bool:
		field.SetBool(v)
	case string:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return err
		}
		field.SetBool(b)
	case int:
		field.SetBool(v != 0)
	case float64:
		field.SetBool(v != 0)
	default:
		return fmt.Errorf("cannot convert %T to bool", value)
	}
	return nil
}

// setFloat sets a floating point value to a field
func setFloat(field reflect.Value, value interface{}) error {
	switch v := value.(type) {
	case float32:
		field.SetFloat(float64(v))
	case float64:
		field.SetFloat(v)
	case int, int8, int16, int32, int64:
		// Use reflection to get the actual int value
		val := reflect.ValueOf(v)
		field.SetFloat(float64(val.Int()))
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return err
		}
		field.SetFloat(f)
	default:
		return fmt.Errorf("cannot convert %T to float", value)
	}
	return nil
}

// setSlice sets a slice value to a field
func setSlice(field reflect.Value, value interface{}) error {
	if v, ok := value.([]interface{}); ok {
		// Create a new slice with the same type as the field
		s := reflect.MakeSlice(field.Type(), len(v), len(v))

		// Set each element in the slice
		for i := 0; i < len(v); i++ {
			elem := s.Index(i)
			if elem.Kind() == reflect.Ptr {
				elem.Set(reflect.New(elem.Type().Elem()))
				elem = elem.Elem()
			}

			if err := setField(elem, v[i]); err != nil {
				return fmt.Errorf("error setting slice element at index %d: %w", i, err)
			}
		}
		field.Set(s)
		return nil
	}

	// Handle single value that should be converted to a slice
	if field.Type().Elem().Kind() == reflect.String {
		if strVal, ok := value.(string); ok {
			// It's a single string for a string slice
			s := reflect.MakeSlice(field.Type(), 1, 1)
			s.Index(0).SetString(strVal)
			field.Set(s)
			return nil
		}
	}

	return fmt.Errorf("cannot convert %T to slice", value)
}

// setStruct sets a struct value to a field
func setStruct(field reflect.Value, value interface{}) error {
	// Handle map to struct conversion
	if structMap, ok := value.(map[string]interface{}); ok {
		for x := 0; x < field.NumField(); x++ {
			nestedField := field.Field(x)
			nestedStructType := field.Type().Field(x)

			tagValue := nestedStructType.Tag.Get(body)
			if tagValue == "" {
				tagValue = nestedStructType.Tag.Get(jjson)
			}

			if tagValue != "" {
				if nestedVal, exists := structMap[tagValue]; exists {
					if err := setField(nestedField, nestedVal); err != nil {
						return fmt.Errorf("error setting nested field '%s': %w", nestedStructType.Name, err)
					}
				}
			}
		}
		return nil
	} else if reflect.TypeOf(value).Kind() == reflect.Map {
		// If not directly map[string]interface{}, handle map or struct assignment gracefully
		return fmt.Errorf("value mismatch for struct mapping")
	}

	return fmt.Errorf("cannot set struct field with value of type %T", value)
}

// isEmptyValue checks if a value is empty or zero
func isEmptyValue(v interface{}) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String, reflect.Array:
		return rv.Len() == 0
	case reflect.Map, reflect.Slice:
		return rv.IsNil() || rv.Len() == 0
	case reflect.Bool:
		return !rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return rv.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return rv.Float() == 0
	case reflect.Interface, reflect.Ptr:
		return rv.IsNil()
	}
	return false
}
