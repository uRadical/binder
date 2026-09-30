package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"uradical.io/go/binder"
)

// User represents a user in our example API
type User struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	Active    bool      `json:"active"`
	Tags      []string  `json:"tags"`
	TeamID    uuid.UUID `json:"team_id"`
	Credit    Money     `json:"credit"`
	CreatedAt time.Time `json:"created_at"`
}

// Money is an amount in pence. It decodes itself from JSON, so binder hands a
// "credit" member to UnmarshalJSON rather than converting it: 12.5 becomes
// 1250 exactly, with no float rounding on the way.
type Money int64

// UnmarshalJSON reads a decimal amount with at most two places, as a JSON
// number (12.50) or a string ("12.50"). A form body or query string gives
// binder text, which it passes on as a JSON string, so the same type binds
// from a form post too.
func (m *Money) UnmarshalJSON(b []byte) error {
	text := strings.Trim(string(b), `"`)
	whole, frac, _ := strings.Cut(text, ".")
	if len(frac) > 2 {
		return fmt.Errorf("%s has more than two decimal places", text)
	}
	pounds, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return fmt.Errorf("%s is not an amount", text)
	}
	pence := int64(0)
	if frac != "" {
		frac += strings.Repeat("0", 2-len(frac))
		if pence, err = strconv.ParseInt(frac, 10, 64); err != nil {
			return fmt.Errorf("%s is not an amount", text)
		}
	}
	if strings.HasPrefix(whole, "-") {
		pence = -pence
	}
	*m = Money(pounds*100 + pence)
	return nil
}

// MarshalJSON writes the amount back as a JSON number: 1250 is 12.50.
func (m Money) MarshalJSON() ([]byte, error) {
	sign, v := "", int64(m)
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Appendf(nil, "%s%d.%02d", sign, v/100, v%100), nil
}

// The teams users belong to. A team is named by a UUID, which binds from any
// source because uuid.UUID implements encoding.TextUnmarshaler.
var (
	teamPlatform = uuid.MustParse("0192f4a0-7b3c-7d4e-9a1b-2c3d4e5f6a70")
	teamSupport  = uuid.MustParse("0192f4a0-7b3c-7d4e-9a1b-2c3d4e5f6a71")
)

// In-memory store for the example. net/http serves requests concurrently, so
// every access to users and nextID holds mu.
var mu sync.Mutex
var users = map[int]User{
	1: {ID: 1, Name: "Alice", Email: "alice@example.com", Active: true, Tags: []string{"admin", "user"}, TeamID: teamPlatform, Credit: 2500, CreatedAt: time.Now().Add(-24 * time.Hour)},
	2: {ID: 2, Name: "Bob", Email: "bob@example.com", Active: false, Tags: []string{"user"}, TeamID: teamSupport, CreatedAt: time.Now().Add(-48 * time.Hour)},
}
var nextID = 3

// Request/Response types demonstrating binder usage

type GetUserRequest struct {
	ID int `path:"id"`
	// Headers bind like any other source.
	TraceID string `header:"X-Request-ID"`
}

// Paging is shared by every endpoint that lists. A request type embeds it, and
// binder promotes its fields as encoding/json does, so ?page=2&limit=5 binds
// into Page and Limit without repeating the tags on each request type.
type Paging struct {
	Page  int `query:"page"`
	Limit int `query:"limit"`
}

// offsets returns the slice bounds of the requested page within n items,
// applying the defaults for a page or limit the client left out.
func (p Paging) offsets(n int) (start, end, limit int) {
	page, limit := max(p.Page, 1), p.Limit
	if limit <= 0 {
		limit = 10
	}
	start = min((page-1)*limit, n)
	return start, min(start+limit, n), limit
}

type ListUsersRequest struct {
	Paging

	// An absent or empty query value leaves a field alone: Active stays nil
	// unless the client sends it.
	Active *bool  `query:"active"`
	APIKey string `cookie:"api_key"`
	// A repeated parameter fills a slice: ?tags=admin&tags=user
	Tags []string `query:"tags"`
	// A pointer tells "no filter" from a team: nil unless ?team= names one,
	// and a value that is not a UUID is reported like any other bad value.
	Team *uuid.UUID `query:"team"`
	// A map binds from name[key]=value pairs: ?filter[name]=ali&filter[email]=example
	// gives {"name": "ali", "email": "example"}. Nil when no filter is sent.
	Filter map[string]string `query:"filter"`
	// A duration binds from text: ?created_within=36h lists users created in
	// the last 36 hours. A pointer, so no parameter means no filter.
	CreatedWithin *time.Duration `query:"created_within"`
}

// filterFields are the user fields ?filter[...] can match, by substring.
var filterFields = map[string]func(User) string{
	"name":  func(u User) string { return u.Name },
	"email": func(u User) string { return u.Email },
}

// Validate rejects a filter on a field that cannot be filtered, naming it as
// the client sent it, so a typo is reported rather than matching everyone.
func (r ListUsersRequest) Validate(ctx context.Context) error {
	errs := ValidationErrors{}
	if r.CreatedWithin != nil && *r.CreatedWithin <= 0 {
		errs["created_within"] = "must be a positive duration"
	}
	for key := range r.Filter {
		if filterFields[key] == nil {
			errs["filter["+key+"]"] = "is not a field that can be filtered"
		}
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

// matchesFilter reports whether a user matches every filter, each a
// case-insensitive substring of the named field.
func matchesFilter(u User, filter map[string]string) bool {
	for key, want := range filter {
		if !strings.Contains(strings.ToLower(filterFields[key](u)), strings.ToLower(want)) {
			return false
		}
	}
	return true
}

type CreateUserRequest struct {
	// required reports a missing value rather than binding a zero one.
	Name   string   `body:"name,required"`
	Email  string   `body:"email,required"`
	Active bool     `body:"active"`
	Tags   []string `body:"tags"`
	// A JSON string such as "0192f4a0-7b3c-7d4e-9a1b-2c3d4e5f6a70" binds
	// straight into a uuid.UUID.
	TeamID uuid.UUID `body:"team_id"`
	// Money decodes itself: {"credit": 12.50} binds as 1250 pence.
	Credit Money `body:"credit"`
}

// ValidationErrors is this application's error type for Validate: problems
// keyed by the field name the client sent. A type of its own lets
// writeBindError recognise it with errors.As.
type ValidationErrors map[string]string

func (v ValidationErrors) Error() string {
	return fmt.Sprintf("%d fields failed validation", len(v))
}

// Validate implements the binder.Validator interface. Presence is handled by
// the required tag, so validation is left for rules binding cannot express.
func (r CreateUserRequest) Validate(ctx context.Context) error {
	errs := ValidationErrors{}
	if !strings.Contains(r.Email, "@") {
		errs["email"] = "is not a valid address"
	}
	if len(r.Tags) > 5 {
		errs["tags"] = "must have at most 5 entries"
	}
	if r.Credit < 0 {
		errs["credit"] = "must not be negative"
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

type UpdateUserRequest struct {
	ID    int    `path:"id"`
	Name  string `body:"name,omitempty"`
	Email string `body:"email,omitempty"`
	// No omitempty: it would skip a JSON false as empty. The pointer alone
	// tells "not sent" (nil) from false.
	Active *bool    `body:"active"`
	Tags   []string `body:"tags,omitempty"`
}

// writeBindError answers a request that failed to bind. An application writes
// this once; the response format and status codes are its own choice.
func writeBindError(w http.ResponseWriter, err error) {
	var bindErrs binder.BindErrors
	var valErrs ValidationErrors

	switch {
	case errors.As(err, &bindErrs):
		// Every field that failed, keyed by the name the client sent.
		fields := map[string]string{}
		for _, e := range bindErrs {
			switch {
			case errors.Is(e, binder.ErrMissingRequired):
				fields[e.Name] = "required"
			case errors.Is(e, binder.ErrUnknownField):
				fields[e.Name] = "unknown field"
			default:
				fields[e.Name] = "invalid value"
			}
		}
		respondJSON(w, map[string]any{"errors": fields}, http.StatusBadRequest)

	case errors.As(err, &valErrs):
		respondJSON(w, map[string]any{"errors": valErrs}, http.StatusUnprocessableEntity)

	case errors.Is(err, binder.ErrBodyTooLarge):
		respondError(w, "request body too large", http.StatusRequestEntityTooLarge)

	case errors.Is(err, binder.ErrInvalidTarget):
		// A bug in this handler, not a bad request.
		log.Printf("bind target is wrong: %v", err)
		respondError(w, "internal server error", http.StatusInternalServerError)

	default:
		respondError(w, err.Error(), http.StatusBadRequest)
	}
}

// HTTP Handlers

func getUser(w http.ResponseWriter, r *http.Request) {
	var req GetUserRequest
	if err := binder.Bind(r, &req); err != nil {
		writeBindError(w, err)
		return
	}

	mu.Lock()
	defer mu.Unlock()

	user, exists := users[req.ID]
	if !exists {
		respondError(w, "User not found", http.StatusNotFound)
		return
	}

	respondJSON(w, user, http.StatusOK)
}

func listUsers(w http.ResponseWriter, r *http.Request) {
	var req ListUsersRequest
	if err := binder.Bind(r, &req); err != nil {
		writeBindError(w, err)
		return
	}

	// Simple API key check for demonstration
	if req.APIKey != "demo-key" {
		respondError(w, "Invalid API key", http.StatusUnauthorized)
		return
	}

	mu.Lock()
	defer mu.Unlock()

	var result []User
	for _, user := range users {
		// Filter by active status if provided
		if req.Active != nil && user.Active != *req.Active {
			continue
		}

		// Filter by any of the repeated tags parameters, if given
		if len(req.Tags) > 0 && !hasAnyTag(user, req.Tags) {
			continue
		}

		// Filter by team, if given
		if req.Team != nil && user.TeamID != *req.Team {
			continue
		}

		// Filter by how recently the user was created, if given
		if req.CreatedWithin != nil && time.Since(user.CreatedAt) > *req.CreatedWithin {
			continue
		}

		// Filter by ?filter[field]=text, if given
		if !matchesFilter(user, req.Filter) {
			continue
		}

		result = append(result, user)
	}

	// Map order is random, so sort before paging for pages to be stable.
	slices.SortFunc(result, func(a, b User) int { return a.ID - b.ID })
	total := len(result)
	start, end, limit := req.offsets(total)

	respondJSON(w, map[string]any{
		"users": result[start:end],
		"count": end - start,
		"total": total,
		"page":  max(req.Page, 1),
		"limit": limit,
	}, http.StatusOK)
}

func createUser(w http.ResponseWriter, r *http.Request) {
	var req CreateUserRequest
	// Per-call options: a tighter body limit than the package default, and
	// refuse a body carrying keys nothing binds, so typos are reported
	// rather than ignored.
	opts := binder.BindOptions{
		MaxBodySize:           64 << 10,
		DisallowUnknownFields: true,
	}
	if err := binder.BindWithOptions(r, &req, opts); err != nil {
		writeBindError(w, err)
		return
	}

	mu.Lock()
	defer mu.Unlock()

	// Create new user
	user := User{
		ID:        nextID,
		Name:      req.Name,
		Email:     req.Email,
		Active:    req.Active,
		Tags:      req.Tags,
		TeamID:    req.TeamID,
		Credit:    req.Credit,
		CreatedAt: time.Now(),
	}

	users[nextID] = user
	nextID++

	respondJSON(w, user, http.StatusCreated)
}

func updateUser(w http.ResponseWriter, r *http.Request) {
	var req UpdateUserRequest
	if err := binder.Bind(r, &req); err != nil {
		writeBindError(w, err)
		return
	}

	mu.Lock()
	defer mu.Unlock()

	user, exists := users[req.ID]
	if !exists {
		respondError(w, "User not found", http.StatusNotFound)
		return
	}

	// Update fields if provided (omitempty allows partial updates)
	if req.Name != "" {
		user.Name = req.Name
	}
	if req.Email != "" {
		user.Email = req.Email
	}
	if req.Active != nil {
		user.Active = *req.Active
	}
	if req.Tags != nil {
		user.Tags = req.Tags
	}

	users[req.ID] = user
	respondJSON(w, user, http.StatusOK)
}

func deleteUser(w http.ResponseWriter, r *http.Request) {
	var req GetUserRequest
	if err := binder.Bind(r, &req); err != nil {
		writeBindError(w, err)
		return
	}

	mu.Lock()
	defer mu.Unlock()

	if _, exists := users[req.ID]; !exists {
		respondError(w, "User not found", http.StatusNotFound)
		return
	}

	delete(users, req.ID)
	w.WriteHeader(http.StatusNoContent)
}

// hasAnyTag reports whether the user carries any of the given tags.
func hasAnyTag(u User, tags []string) bool {
	for _, want := range tags {
		for _, has := range u.Tags {
			if has == want {
				return true
			}
		}
	}
	return false
}

// Helper functions

func respondJSON(w http.ResponseWriter, data any, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func respondError(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// Middleware to set API key cookie for demo purposes
func demoMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set demo API key cookie if not present
		if _, err := r.Cookie("api_key"); err != nil {
			http.SetCookie(w, &http.Cookie{
				Name:  "api_key",
				Value: "demo-key",
				Path:  "/",
			})
		}
		next.ServeHTTP(w, r)
	})
}

// routes wires the API's handlers, wrapped in the demo middleware.
func routes() http.Handler {
	mux := http.NewServeMux()

	// API routes demonstrating different binding scenarios
	mux.HandleFunc("GET /users/{id}", getUser)       // Path parameter
	mux.HandleFunc("GET /users", listUsers)          // Query parameters + cookies
	mux.HandleFunc("POST /users", createUser)        // JSON body + validation
	mux.HandleFunc("PUT /users/{id}", updateUser)    // Path + body (partial updates)
	mux.HandleFunc("DELETE /users/{id}", deleteUser) // Path parameter

	return demoMiddleware(mux)
}

func main() {
	handler := routes()

	fmt.Println("🚀 Binder Example Server starting on :8080")
	fmt.Println()
	fmt.Println("Try these examples:")
	fmt.Println("  GET    http://localhost:8080/users/1")
	fmt.Println("  GET    http://localhost:8080/users?active=true&limit=5")
	fmt.Println("  GET    http://localhost:8080/users?page=2&limit=1")
	fmt.Println("  GET    http://localhost:8080/users?tags=admin&tags=user")
	fmt.Println("  GET    http://localhost:8080/users?team=" + teamPlatform.String())
	fmt.Println("  GET    http://localhost:8080/users?filter[name]=ali")
	fmt.Println("  GET    http://localhost:8080/users?created_within=36h")
	fmt.Println("  POST   http://localhost:8080/users")
	fmt.Println("  PUT    http://localhost:8080/users/1")
	fmt.Println("  DELETE http://localhost:8080/users/1")
	fmt.Println()
	fmt.Println("GET /users needs the api_key cookie: a browser gets it from its first")
	fmt.Println("response; with curl, add -b api_key=demo-key and quote the URL, and")
	fmt.Println("add -g for a URL with [ ], such as filter[name].")
	fmt.Println()
	fmt.Println("See example/README.md for detailed usage instructions")

	log.Fatal(http.ListenAndServe(":8080", handler))
}
