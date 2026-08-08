package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pgeske/filmstream/internal/resolver"
)

type fakeResolver struct{}

func (fakeResolver) Resolve(_ context.Context, input string) (resolver.Result, error) {
	return resolver.Result{Input: input, Candidates: []resolver.Candidate{{Title: "The Movie", Year: 2001, Confidence: 0.9}}}, nil
}

func TestResolveMovie(t *testing.T) {
	server := &Server{movieResolver: fakeResolver{}}
	request := httptest.NewRequest(http.MethodPost, "/v1/resolve", strings.NewReader(`{"query":"rough description"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "The Movie") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestReloadIndexerConfiguration(t *testing.T) {
	called := false
	server := &Server{reloadIndexers: func() error {
		called = true
		return nil
	}}
	request := httptest.NewRequest(http.MethodPost, "/v1/indexers/reload", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !called {
		t.Fatal("reload callback was not called")
	}
}
