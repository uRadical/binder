package binder

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// CustomTime implements TextUnmarshaler for testing
type CustomTime struct {
	Time time.Time
}

func (ct *CustomTime) UnmarshalText(text []byte) error {
	t, err := time.Parse("2006-01-02", string(text))
	if err != nil {
		return err
	}
	ct.Time = t
	return nil
}

// ValidationStruct implements Validator for testing
type ValidationStruct struct {
	Value int `path:"value"`
}

func (v ValidationStruct) Validate() error {
	if v.Value < 0 {
		return fmt.Errorf("value must be positive")
	}
	return nil
}

func TestBindInt(t *testing.T) {
	r := httptest.NewRequest("GET", "/test", nil)
	r.SetPathValue("id", "123")

	type params struct {
		ID int `path:"id"`
	}

	var p params
	err := Bind(r, &p)
	if err != nil {
		t.Errorf("Binding failed with error: %v", err)
	}

	if p.ID != 123 {
		t.Errorf("Expected ID to be 123, got %d", p.ID)
	}
}

func TestBindUUID(t *testing.T) {
	r := httptest.NewRequest("GET", "/test", nil)
	r.SetPathValue("id", "f47ac10b-58cc-0372-8562-0b8e853961a1")

	type params struct {
		ID uuid.UUID `path:"id"`
	}

	var p params
	err := Bind(r, &p)
	if err != nil {
		t.Errorf("Binding failed with error: %v", err)
	}

	expectedUUID := "f47ac10b-58cc-0372-8562-0b8e853961a1"
	if p.ID.String() != expectedUUID {
		t.Errorf("Expected ID to be %s, got %s", expectedUUID, p.ID.String())
	}
}

func TestBindQuery(t *testing.T) {
	r := httptest.NewRequest("GET", "/test?name=Hecate&count=42&price=19.99&flag=true", nil)

	type params struct {
		Name  string  `query:"name"`
		Count int     `query:"count"`
		Price float64 `query:"price"`
		Flag  bool    `query:"flag"`
	}

	var p params
	err := Bind(r, &p)
	if err != nil {
		t.Errorf("Binding failed with error: %v", err)
	}

	if p.Name != "Hecate" {
		t.Errorf("Expected Name to be Hecate, got %s", p.Name)
	}
	if p.Count != 42 {
		t.Errorf("Expected Count to be 42, got %d", p.Count)
	}
	if p.Price != 19.99 {
		t.Errorf("Expected Price to be 19.99, got %f", p.Price)
	}
	if !p.Flag {
		t.Errorf("Expected Flag to be true, got %v", p.Flag)
	}
}

func TestBindCookie(t *testing.T) {
	r := httptest.NewRequest("GET", "/test", nil)
	r.AddCookie(&http.Cookie{Name: "token", Value: "abc123"})
	r.AddCookie(&http.Cookie{Name: "user_id", Value: "456"})

	type params struct {
		Token  string `cookie:"token"`
		UserID int    `cookie:"user_id"`
	}

	var p params
	err := Bind(r, &p)
	if err != nil {
		t.Errorf("Binding failed with error: %v", err)
	}

	if p.Token != "abc123" {
		t.Errorf("Expected Token to be abc123, got %s", p.Token)
	}
	if p.UserID != 456 {
		t.Errorf("Expected UserID to be 456, got %d", p.UserID)
	}
}

func TestBindTextUnmarshaler(t *testing.T) {
	r := httptest.NewRequest("GET", "/test", nil)
	r.SetPathValue("date", "2023-05-15")

	type params struct {
		Date CustomTime `path:"date"`
	}

	var p params
	err := Bind(r, &p)
	if err != nil {
		t.Errorf("Binding failed with error: %v", err)
	}

	if p.Date.Time.Year() != 2023 {
		t.Errorf("Expected year to be 2023, got %d", p.Date.Time.Year())
	}
	if p.Date.Time.Month() != time.Month(5) {
		t.Errorf("Expected month to be 5, got %d", p.Date.Time.Month())
	}
	if p.Date.Time.Day() != 15 {
		t.Errorf("Expected day to be 15, got %d", p.Date.Time.Day())
	}
}

func TestBindValidationSuccess(t *testing.T) {
	r := httptest.NewRequest("GET", "/test", nil)
	r.SetPathValue("value", "10")

	var p ValidationStruct
	err := Bind(r, &p)
	if err != nil {
		t.Errorf("Binding should succeed with positive value, got error: %v", err)
	}

	if p.Value != 10 {
		t.Errorf("Expected Value to be 10, got %d", p.Value)
	}
}

func TestBindValidationFailure(t *testing.T) {
	r := httptest.NewRequest("GET", "/test", nil)
	r.SetPathValue("value", "-10")

	var p ValidationStruct
	err := Bind(r, &p)
	if err == nil {
		t.Errorf("Binding should fail with negative value")
	}

	if !strings.Contains(err.Error(), "validation failed") {
		t.Errorf("Expected validation error, got: %v", err)
	}
}

func TestBindOmitEmpty(t *testing.T) {
	r := httptest.NewRequest("GET", "/test?name=TestName", nil)

	type params struct {
		ID   int    `query:"id,omitempty"`
		Name string `query:"name,omitempty"`
	}

	var p params
	p.ID = 999 // Default value
	p.Name = "DefaultName"

	err := Bind(r, &p)
	if err != nil {
		t.Errorf("Binding failed with error: %v", err)
	}

	if p.ID != 999 {
		t.Errorf("Expected ID to remain 999, got %d", p.ID)
	}
	if p.Name != "TestName" {
		t.Errorf("Expected Name to be TestName, got %s", p.Name)
	}
}

func TestBindJsonBody(t *testing.T) {
	type nested struct {
		NEmail string `body:"email"`
		Count  int    `body:"count"`
	}

	type params struct {
		UID    uuid.UUID `body:"uid"`
		Email  string    `body:"email"`
		Active bool      `body:"active"`
		Amount float64   `body:"amount"`
		Nested nested    `body:"nested"`
		Nums   []int     `body:"nums"`
		Tags   []string  `body:"tags"`
	}

	payload := map[string]interface{}{
		"email":  "info@example.io",
		"uid":    "f47ac10b-58cc-0372-8562-0b8e853961a1",
		"active": true,
		"amount": 99.99,
		"nested": map[string]interface{}{
			"email": "nested@example.io",
			"count": 42,
		},
		"nums": []int{13, 24, 35},
		"tags": []string{"tag1", "tag2", "tag3"},
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Failed to marshal JSON: %v", err)
	}

	r := httptest.NewRequest("POST", "/test", bytes.NewBuffer(payloadBytes))
	r.Header.Set("Content-Type", "application/json")

	var p params
	err = Bind(r, &p)
	if err != nil {
		t.Errorf("Binding failed with error: %v", err)
	}

	if p.UID.String() != "f47ac10b-58cc-0372-8562-0b8e853961a1" {
		t.Errorf("Expected UID to be f47ac10b-58cc-0372-8562-0b8e853961a1, got %s", p.UID)
	}
	if p.Email != "info@example.io" {
		t.Errorf("Expected Email to be info@example.io, got %s", p.Email)
	}
	if !p.Active {
		t.Errorf("Expected Active to be true, got %v", p.Active)
	}
	if p.Amount != 99.99 {
		t.Errorf("Expected Amount to be 99.99, got %f", p.Amount)
	}
	if p.Nested.NEmail != "nested@example.io" {
		t.Errorf("Expected Nested.NEmail to be nested@example.io, got %s", p.Nested.NEmail)
	}
	if p.Nested.Count != 42 {
		t.Errorf("Expected Nested.Count to be 42, got %d", p.Nested.Count)
	}

	expectedNums := []int{13, 24, 35}
	if !reflect.DeepEqual(p.Nums, expectedNums) {
		t.Errorf("Expected Nums to be %v, got %v", expectedNums, p.Nums)
	}

	expectedTags := []string{"tag1", "tag2", "tag3"}
	if !reflect.DeepEqual(p.Tags, expectedTags) {
		t.Errorf("Expected Tags to be %v, got %v", expectedTags, p.Tags)
	}
}

func TestBindFormBody(t *testing.T) {
	formData := url.Values{}
	formData.Add("email", "test@example.io")
	formData.Add("flag", "false")
	formData.Add("count", "42")
	formData.Add("amount", "99.99")

	r := httptest.NewRequest("POST", "/test", strings.NewReader(formData.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	type params struct {
		Email  string  `body:"email"`
		Flag   bool    `body:"flag"`
		Count  int     `body:"count"`
		Amount float64 `body:"amount"`
	}

	var p params
	err := Bind(r, &p)
	if err != nil {
		t.Errorf("Binding failed with error: %v", err)
	}

	if p.Email != "test@example.io" {
		t.Errorf("Expected Email to be test@example.io, got %s", p.Email)
	}
	if p.Flag {
		t.Errorf("Expected Flag to be false, got %v", p.Flag)
	}
	if p.Count != 42 {
		t.Errorf("Expected Count to be 42, got %d", p.Count)
	}
	if p.Amount != 99.99 {
		t.Errorf("Expected Amount to be 99.99, got %f", p.Amount)
	}
}

func TestBindMultipleBodyReads(t *testing.T) {
	payload := map[string]interface{}{
		"name":   "Test User",
		"email":  "test@example.com",
		"age":    30,
		"active": true,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Failed to marshal JSON: %v", err)
	}

	r := httptest.NewRequest("POST", "/test", bytes.NewBuffer(payloadBytes))
	r.Header.Set("Content-Type", "application/json")

	// First binding
	type params1 struct {
		Name  string `body:"name"`
		Email string `body:"email"`
	}
	var p1 params1
	err = Bind(r, &p1)
	if err != nil {
		t.Errorf("First binding failed with error: %v", err)
	}

	if p1.Name != "Test User" {
		t.Errorf("Expected Name to be Test User, got %s", p1.Name)
	}
	if p1.Email != "test@example.com" {
		t.Errorf("Expected Email to be test@example.com, got %s", p1.Email)
	}

	// Second binding - should still work since body is restored
	type params2 struct {
		Age    int  `body:"age"`
		Active bool `body:"active"`
	}
	var p2 params2
	err = Bind(r, &p2)
	if err != nil {
		t.Errorf("Second binding failed with error: %v", err)
	}

	if p2.Age != 30 {
		t.Errorf("Expected Age to be 30, got %d", p2.Age)
	}
	if !p2.Active {
		t.Errorf("Expected Active to be true, got %v", p2.Active)
	}
}

func TestBindInvalidInt(t *testing.T) {
	r := httptest.NewRequest("GET", "/test", nil)
	r.SetPathValue("id", "not-an-int")

	type params struct {
		ID int `path:"id"`
	}

	var p params
	err := Bind(r, &p)
	if err == nil {
		t.Errorf("Binding should fail with invalid int")
	}

	if !strings.Contains(err.Error(), "error setting field ID") {
		t.Errorf("Expected error about field ID, got: %v", err)
	}
}

func TestBindInvalidFloat(t *testing.T) {
	r := httptest.NewRequest("GET", "/test?value=not-a-float", nil)

	type params struct {
		Value float64 `query:"value"`
	}

	var p params
	err := Bind(r, &p)
	if err == nil {
		t.Errorf("Binding should fail with invalid float")
	}

	if !strings.Contains(err.Error(), "error setting field Value") {
		t.Errorf("Expected error about field Value, got: %v", err)
	}
}

func TestBindInvalidBool(t *testing.T) {
	r := httptest.NewRequest("GET", "/test?flag=not-a-bool", nil)

	type params struct {
		Flag bool `query:"flag"`
	}

	var p params
	err := Bind(r, &p)
	if err == nil {
		t.Errorf("Binding should fail with invalid boolean")
	}

	if !strings.Contains(err.Error(), "error setting field Flag") {
		t.Errorf("Expected error about field Flag, got: %v", err)
	}
}

func TestBindInvalidUUID(t *testing.T) {
	r := httptest.NewRequest("GET", "/test", nil)
	r.SetPathValue("id", "not-a-uuid")

	type params struct {
		ID uuid.UUID `path:"id"`
	}

	var p params
	err := Bind(r, &p)
	if err == nil {
		t.Errorf("Binding should fail with invalid UUID")
	}

	if !strings.Contains(err.Error(), "error setting field ID") {
		t.Errorf("Expected error about field ID, got: %v", err)
	}
}

func TestBindUnsupportedType(t *testing.T) {
	r := httptest.NewRequest("GET", "/test?value=123", nil)

	type params struct {
		Value complex128 `query:"value"`
	}

	var p params
	err := Bind(r, &p)
	if err == nil {
		t.Errorf("Binding should fail with unsupported type")
	}

	if !strings.Contains(err.Error(), "unsupported type") {
		t.Errorf("Expected error about unsupported type, got: %v", err)
	}
}

func TestBindArrayNotSupported(t *testing.T) {
	payload := map[string]interface{}{
		"values": []int{1, 2, 3},
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Failed to marshal JSON: %v", err)
	}

	r := httptest.NewRequest("POST", "/test", bytes.NewBuffer(payloadBytes))
	r.Header.Set("Content-Type", "application/json")

	type params struct {
		Values [3]int `body:"values"`
	}

	var p params
	err = Bind(r, &p)
	if err == nil {
		t.Errorf("Binding should fail with array type")
	}

	if !strings.Contains(err.Error(), "arrays are not supported") {
		t.Errorf("Expected error about arrays not supported, got: %v", err)
	}
}

func TestFieldCache(t *testing.T) {
	type cachedStruct struct {
		ID   int    `path:"id"`
		Name string `query:"name"`
	}

	// Clear the cache before the test
	fieldCacheMutex.Lock()
	delete(fieldCache, reflect.TypeOf(cachedStruct{}))
	fieldCacheMutex.Unlock()

	// First access - should build cache
	info1 := getFieldInfo(reflect.TypeOf(cachedStruct{}))
	if len(info1) != 2 {
		t.Errorf("Expected 2 field info entries, got %d", len(info1))
	}

	// Second access - should use cache
	info2 := getFieldInfo(reflect.TypeOf(cachedStruct{}))

	// Check equality of the two maps
	if len(info1) != len(info2) {
		t.Errorf("Expected info1 and info2 to have the same length")
	}

	// Manual deep equality check
	for k, v1 := range info1 {
		v2, exists := info2[k]
		if !exists {
			t.Errorf("Key %s exists in info1 but not in info2", k)
		}

		if v1.Index != v2.Index || v1.Source != v2.Source || v1.TagName != v2.TagName || v1.OmitEmpty != v2.OmitEmpty {
			t.Errorf("Values for key %s differ between info1 and info2", k)
		}
	}

	// Check the cache directly
	fieldCacheMutex.RLock()
	cachedInfo, exists := fieldCache[reflect.TypeOf(cachedStruct{})]
	fieldCacheMutex.RUnlock()

	if !exists {
		t.Errorf("Type should exist in cache")
	}

	// Check if cached info equals the returned info
	if len(info1) != len(cachedInfo) {
		t.Errorf("Expected cachedInfo to have the same length as info1")
	}

	for k, v1 := range info1 {
		v2, exists := cachedInfo[k]
		if !exists {
			t.Errorf("Key %s exists in info1 but not in cachedInfo", k)
		}

		if v1.Index != v2.Index || v1.Source != v2.Source || v1.TagName != v2.TagName || v1.OmitEmpty != v2.OmitEmpty {
			t.Errorf("Values for key %s differ between info1 and cachedInfo", k)
		}
	}
}

func TestContentTypeParser(t *testing.T) {
	tests := []struct {
		header   string
		expected string
	}{
		{"application/json", "application/json"},
		{"application/json; charset=utf-8", "application/json"},
		{"application/json;charset=utf-8", "application/json"},
		{"text/plain", "text/plain"},
		{"text/plain; charset=iso-8859-1", "text/plain"},
		{"", ""},
		{"  application/json  ; charset=utf-8", "application/json"},
	}

	for _, tt := range tests {
		result := parseContentType(tt.header)
		if result != tt.expected {
			t.Errorf("parseContentType(%q) = %q, want %q", tt.header, result, tt.expected)
		}
	}
}

func BenchmarkBind(b *testing.B) {
	// Test type for binding
	type params struct {
		ID     int       `path:"id"`
		Name   string    `query:"name"`
		Email  string    `body:"email"`
		Active bool      `body:"active"`
		UUID   uuid.UUID `body:"uuid"`
	}

	// Create a sample HTTP request
	payload := map[string]interface{}{
		"email":  "test@example.com",
		"active": true,
		"uuid":   "f47ac10b-58cc-0372-8562-0b8e853961a1",
	}

	payloadBytes, _ := json.Marshal(payload)

	req := httptest.NewRequest("POST", "/test?name=TestUser", bytes.NewBuffer(payloadBytes))
	req.Header.Set("Content-Type", "application/json")

	// Simulate path parameters
	req.SetPathValue("id", "123")

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		var p params
		_ = Bind(req, &p)
	}
}

func BenchmarkBindWithoutCache(b *testing.B) {
	// For comparison - clear cache on each iteration

	// Test type for binding
	type params struct {
		ID     int       `path:"id"`
		Name   string    `query:"name"`
		Email  string    `body:"email"`
		Active bool      `body:"active"`
		UUID   uuid.UUID `body:"uuid"`
	}

	// Create a sample HTTP request
	payload := map[string]interface{}{
		"email":  "test@example.com",
		"active": true,
		"uuid":   "f47ac10b-58cc-0372-8562-0b8e853961a1",
	}

	payloadBytes, _ := json.Marshal(payload)

	req := httptest.NewRequest("POST", "/test?name=TestUser", bytes.NewBuffer(payloadBytes))
	req.Header.Set("Content-Type", "application/json")

	// Simulate path parameters
	req.SetPathValue("id", "123")

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Clear cache for each iteration
		fieldCacheMutex.Lock()
		fieldCache = make(map[reflect.Type]map[string]fieldInfo)
		fieldCacheMutex.Unlock()

		var p params
		_ = Bind(req, &p)
	}
}
