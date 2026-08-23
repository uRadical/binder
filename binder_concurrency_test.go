package binder

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// The field cache is shared mutable state guarded by a double-checked
// RWMutex. These tests contend on it deliberately, and are meaningful only
// under -race, which CI runs.

// distinctTypes builds struct types at runtime so that goroutines contend on
// cache writes rather than settling into the read path after the first bind.
func distinctTypes(n int) []reflect.Type {
	types := make([]reflect.Type, n)
	for i := range types {
		types[i] = reflect.StructOf([]reflect.StructField{
			{
				Name: "Q",
				Type: reflect.TypeOf(""),
				Tag:  reflect.StructTag(fmt.Sprintf(`query:"q%d"`, i)),
			},
			{
				Name: "B",
				Type: reflect.TypeOf(0),
				Tag:  reflect.StructTag(fmt.Sprintf(`body:"b%d,omitempty"`, i)),
			},
			{
				Name: "H",
				Type: reflect.TypeOf(""),
				Tag:  reflect.StructTag(fmt.Sprintf(`header:"X-H%d"`, i)),
			},
		})
	}
	return types
}

func TestConcurrentBindingDistinctTypes(t *testing.T) {
	const goroutines, iterations = 32, 64
	types := distinctTypes(24)

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				typ := types[(g+i)%len(types)]

				r := httptest.NewRequest("POST", "/u?q0=v&q1=v", strings.NewReader(`{"b0":1,"b1":2}`))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("X-H0", "h")

				target := reflect.New(typ).Interface()
				if err := Bind(r, target); err != nil {
					t.Errorf("goroutine %d iteration %d: %v", g, i, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// Binding one type from many goroutines exercises the cache's read path and
// the value each reader receives.
func TestConcurrentBindingSharedType(t *testing.T) {
	type request struct {
		Q string `query:"q"`
		B int    `body:"b"`
		H string `header:"X-H"`
	}

	const goroutines, iterations = 32, 128
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				r := httptest.NewRequest("POST", "/u?q=value", strings.NewReader(`{"b":7}`))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("X-H", "header")

				var got request
				if err := Bind(r, &got); err != nil {
					t.Errorf("bind failed: %v", err)
					return
				}
				if got.Q != "value" || got.B != 7 || got.H != "header" {
					t.Errorf("bound %+v, want all three fields set", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// Clearing the cache while binding forces readers through the rebuild path,
// where the double-checked locking has to hold.
func TestConcurrentBindingWithCacheEviction(t *testing.T) {
	type request struct {
		Q string `query:"q"`
	}

	const goroutines, iterations = 16, 128
	stop := make(chan struct{})

	var evictor sync.WaitGroup
	evictor.Add(1)
	go func() {
		defer evictor.Done()
		for {
			select {
			case <-stop:
				return
			default:
				fieldCacheMutex.Lock()
				fieldCache = make(map[reflect.Type]*typeInfo)
				fieldCacheMutex.Unlock()
			}
		}
	}()

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				var got request
				if err := Bind(httptest.NewRequest("GET", "/u?q=value", nil), &got); err != nil {
					t.Errorf("bind failed: %v", err)
					return
				}
				if got.Q != "value" {
					t.Errorf("Q = %q, want %q", got.Q, "value")
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	evictor.Wait()
}

// Concurrent use through a real server, which is how the package is actually
// reached.
func TestConcurrentBindingThroughServer(t *testing.T) {
	type request struct {
		ID    string `path:"id"`
		Q     string `query:"q"`
		Email string `body:"email,required"`
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		var got request
		if err := Bind(r, &got); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if got.ID == "" || got.Email == "" {
			http.Error(w, "incomplete bind", http.StatusInternalServerError)
			return
		}
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	var wg sync.WaitGroup
	for g := 0; g < 24; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				body := strings.NewReader(`{"email":"a@b.c"}`)
				req, err := http.NewRequest("POST", fmt.Sprintf("%s/users/%d?q=x", srv.URL, g), body)
				if err != nil {
					t.Errorf("building request: %v", err)
					return
				}
				req.Header.Set("Content-Type", "application/json")

				resp, err := srv.Client().Do(req)
				if err != nil {
					t.Errorf("request failed: %v", err)
					return
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Errorf("status %d, want 200", resp.StatusCode)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
