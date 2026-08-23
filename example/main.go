package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"uradical.io/go/binder"
)

// User represents a user in our example API
type User struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	Active    bool      `json:"active"`
	Tags      []string  `json:"tags"`
	CreatedAt time.Time `json:"created_at"`
}

// In-memory store for the example
var users = map[int]User{
	1: {ID: 1, Name: "Alice", Email: "alice@example.com", Active: true, Tags: []string{"admin", "user"}, CreatedAt: time.Now().Add(-24 * time.Hour)},
	2: {ID: 2, Name: "Bob", Email: "bob@example.com", Active: false, Tags: []string{"user"}, CreatedAt: time.Now().Add(-48 * time.Hour)},
}
var nextID = 3

// Request/Response types demonstrating binder usage

type GetUserRequest struct {
	ID int `path:"id"`
	// Headers bind like any other source.
	TraceID string `header:"X-Request-ID"`
}

type ListUsersRequest struct {
	Active *bool  `query:"active,omitempty"`
	Limit  int    `query:"limit,omitempty"`
	APIKey string `cookie:"api_key"`
	// A repeated parameter fills a slice: ?tags=admin&tags=user
	Tags []string `query:"tags,omitempty"`
}

type CreateUserRequest struct {
	// required reports a missing value rather than binding a zero one.
	Name   string   `body:"name,required"`
	Email  string   `body:"email,required"`
	Active bool     `body:"active"`
	Tags   []string `body:"tags"`
}

// Validate implements the binder.Validator interface. Presence is handled by
// the required tag, so validation is left for rules binding cannot express.
func (r CreateUserRequest) Validate() error {
	if !strings.Contains(r.Email, "@") {
		return fmt.Errorf("email %q is not a valid address", r.Email)
	}
	return nil
}

type UpdateUserRequest struct {
	ID     int      `path:"id"`
	Name   string   `body:"name,omitempty"`
	Email  string   `body:"email,omitempty"`
	Active *bool    `body:"active,omitempty"`
	Tags   []string `body:"tags,omitempty"`
}

// bindRequest binds a request and, on failure, answers with a status matching
// what went wrong. Not every binding failure is the client's fault, so they do
// not all deserve a 400.
func bindRequest(w http.ResponseWriter, r *http.Request, target interface{}, opts binder.BindOptions) bool {
	err := binder.BindWithOptions(r, target, opts)
	if err == nil {
		return true
	}

	switch {
	case errors.Is(err, binder.ErrInvalidTarget):
		// A programming error in this handler, not a bad request.
		log.Printf("bind target is wrong: %v", err)
		respondError(w, "internal server error", http.StatusInternalServerError)

	case errors.Is(err, binder.ErrBodyTooLarge):
		respondError(w, err.Error(), http.StatusRequestEntityTooLarge)

	default:
		// A BindError names the field and the input it came from, so the
		// response can tell the client which part of the request to fix
		// rather than handing back one opaque string.
		var bindErr *binder.BindError
		if errors.As(err, &bindErr) {
			respondJSON(w, map[string]interface{}{
				"error":     bindErr.Error(),
				"field":     bindErr.Field,
				"source":    bindErr.Source,
				"parameter": bindErr.Name,
			}, http.StatusBadRequest)
			return false
		}
		respondError(w, err.Error(), http.StatusBadRequest)
	}
	return false
}

// HTTP Handlers

func getUser(w http.ResponseWriter, r *http.Request) {
	var req GetUserRequest
	if !bindRequest(w, r, &req, binder.BindOptions{}) {
		return
	}

	user, exists := users[req.ID]
	if !exists {
		respondError(w, "User not found", http.StatusNotFound)
		return
	}

	respondJSON(w, user, http.StatusOK)
}

func listUsers(w http.ResponseWriter, r *http.Request) {
	var req ListUsersRequest
	if !bindRequest(w, r, &req, binder.BindOptions{}) {
		return
	}

	// Simple API key check for demonstration
	if req.APIKey != "demo-key" {
		respondError(w, "Invalid API key", http.StatusUnauthorized)
		return
	}

	var result []User
	count := 0
	limit := req.Limit
	if limit == 0 {
		limit = 10 // default limit
	}

	for _, user := range users {
		// Filter by active status if provided
		if req.Active != nil && user.Active != *req.Active {
			continue
		}

		// Filter by any of the repeated tags parameters, if given
		if len(req.Tags) > 0 && !hasAnyTag(user, req.Tags) {
			continue
		}

		result = append(result, user)
		count++
		if count >= limit {
			break
		}
	}

	respondJSON(w, map[string]interface{}{
		"users": result,
		"count": len(result),
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
	if !bindRequest(w, r, &req, opts) {
		return
	}

	// Create new user
	user := User{
		ID:        nextID,
		Name:      req.Name,
		Email:     req.Email,
		Active:    req.Active,
		Tags:      req.Tags,
		CreatedAt: time.Now(),
	}

	users[nextID] = user
	nextID++

	respondJSON(w, user, http.StatusCreated)
}

func updateUser(w http.ResponseWriter, r *http.Request) {
	var req UpdateUserRequest
	if !bindRequest(w, r, &req, binder.BindOptions{}) {
		return
	}

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
	if !bindRequest(w, r, &req, binder.BindOptions{}) {
		return
	}

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

func respondJSON(w http.ResponseWriter, data interface{}, status int) {
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

func main() {
	// Cap request bodies for every call that does not override it. The
	// default is 10 MB; this API has no need for anything that large.
	binder.MaxBodySize = 1 << 20

	mux := http.NewServeMux()

	// API routes demonstrating different binding scenarios
	mux.HandleFunc("GET /users/{id}", getUser)       // Path parameter
	mux.HandleFunc("GET /users", listUsers)          // Query parameters + cookies
	mux.HandleFunc("POST /users", createUser)        // JSON body + validation
	mux.HandleFunc("PUT /users/{id}", updateUser)    // Path + body (partial updates)
	mux.HandleFunc("DELETE /users/{id}", deleteUser) // Path parameter

	// Wrap with demo middleware
	handler := demoMiddleware(mux)

	fmt.Println("🚀 Binder Example Server starting on :8080")
	fmt.Println()
	fmt.Println("Try these examples:")
	fmt.Println("  GET    http://localhost:8080/users/1")
	fmt.Println("  GET    http://localhost:8080/users?active=true&limit=5")
	fmt.Println("  GET    http://localhost:8080/users?tags=admin&tags=user")
	fmt.Println("  POST   http://localhost:8080/users")
	fmt.Println("  PUT    http://localhost:8080/users/1")
	fmt.Println("  DELETE http://localhost:8080/users/1")
	fmt.Println()
	fmt.Println("See example/README.md for detailed usage instructions")

	log.Fatal(http.ListenAndServe(":8080", handler))
}
