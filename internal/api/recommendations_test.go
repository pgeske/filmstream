package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pgeske/filmstream/internal/metadata"
	"github.com/pgeske/filmstream/internal/recommendations"
)

type apiRecommendationGeneratorFunc func(context.Context, string) ([]metadata.Movie, error)

func (f apiRecommendationGeneratorFunc) Generate(ctx context.Context, prompt string) ([]metadata.Movie, error) {
	return f(ctx, prompt)
}

func TestRecommendationEndpointsPersistPromptAndReturnEmptyItems(t *testing.T) {
	stateDir := t.TempDir()
	service := recommendations.NewService(
		recommendations.NewStore(stateDir), nil, nil, recommendations.ServiceOptions{},
	)
	server := &Server{recommendationService: service}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/recommendations", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", response.Code, response.Body.String())
	}
	assertEmptyRecommendationItems(t, response.Body.Bytes())

	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(
		http.MethodPut, "/v1/recommendations/prompt", strings.NewReader(`{"prompt":"  slow cinema  "}`),
	))
	if response.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", response.Code, response.Body.String())
	}
	var updated recommendations.Response
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Prompt != "slow cinema" || updated.Refreshing || updated.Items == nil {
		t.Fatalf("PUT response = %+v", updated)
	}

	restarted := recommendations.NewService(
		recommendations.NewStore(stateDir), nil, nil, recommendations.ServiceOptions{},
	)
	persisted := restarted.Snapshot()
	if persisted.Prompt != "slow cinema" {
		t.Fatalf("saved prompt = %q", persisted.Prompt)
	}

	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/recommendations/refresh", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", response.Code, response.Body.String())
	}
	assertEmptyRecommendationItems(t, response.Body.Bytes())
}

func TestRecommendationEndpointsKeepPersistedResponseShape(t *testing.T) {
	stateDir := t.TempDir()
	service := recommendations.NewService(
		recommendations.NewStore(stateDir),
		apiRecommendationGeneratorFunc(func(context.Context, string) ([]metadata.Movie, error) {
			return []metadata.Movie{
				{ID: "tmdb-tv:1", MediaType: metadata.MediaTypeShow, Title: "Recommended Show"},
				{ID: "tmdb:1", MediaType: metadata.MediaTypeMovie, Title: "Recommended Movie"},
			}, nil
		}),
		nil,
		recommendations.ServiceOptions{},
	)
	if _, err := service.SetPrompt("balanced taste"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for service.Snapshot().Refreshing {
		if time.Now().After(deadline) {
			t.Fatal("recommendation refresh did not finish")
		}
		time.Sleep(time.Millisecond)
	}

	persistedService := recommendations.NewService(
		recommendations.NewStore(stateDir), nil, nil, recommendations.ServiceOptions{},
	)
	server := &Server{recommendationService: persistedService}
	requests := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/v1/recommendations", nil),
		httptest.NewRequest(
			http.MethodPut, "/v1/recommendations/prompt", strings.NewReader(`{"prompt":"balanced taste"}`),
		),
	}
	for _, request := range requests {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", request.Method, response.Code, response.Body.String())
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload) != 4 {
			t.Fatalf("%s response keys = %v", request.Method, payload)
		}
		for _, key := range []string{"generated_at", "prompt", "refreshing", "items"} {
			if _, exists := payload[key]; !exists {
				t.Fatalf("%s response is missing %q: %s", request.Method, key, response.Body.String())
			}
		}
		var decoded recommendations.Response
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Prompt != "balanced taste" || decoded.Refreshing || len(decoded.Items) != 2 ||
			decoded.Items[0].MediaType != metadata.MediaTypeShow ||
			decoded.Items[1].MediaType != metadata.MediaTypeMovie {
			t.Fatalf("%s response = %+v", request.Method, decoded)
		}
	}
}

func TestRecommendationPromptEndpointRejectsOversizedAndUnknownInput(t *testing.T) {
	service := recommendations.NewService(
		recommendations.NewStore(t.TempDir()), nil, nil, recommendations.ServiceOptions{},
	)
	server := &Server{recommendationService: service}

	for _, body := range []string{
		`{"prompt":"ok","unknown":true}`,
		`{"prompt":"` + strings.Repeat("a", recommendations.MaxPromptBytes+1) + `"}`,
	} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(
			http.MethodPut, "/v1/recommendations/prompt", strings.NewReader(body),
		))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	}
	if service.Snapshot().Prompt != "" {
		t.Fatalf("prompt changed after invalid requests: %q", service.Snapshot().Prompt)
	}
}

func TestRecommendationEndpointsDegradeWithoutService(t *testing.T) {
	server := &Server{}
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/v1/recommendations", nil),
		httptest.NewRequest(http.MethodPost, "/v1/recommendations/refresh", nil),
	} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		assertEmptyRecommendationItems(t, response.Body.Bytes())
	}
}

func assertEmptyRecommendationItems(t *testing.T, body []byte) {
	t.Helper()
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload["items"], []byte("[]")) {
		t.Fatalf("items JSON = %s", payload["items"])
	}
	if _, exists := payload["prompt"]; !exists {
		t.Fatal("prompt is missing")
	}
	if _, exists := payload["refreshing"]; !exists {
		t.Fatal("refreshing is missing")
	}
}
