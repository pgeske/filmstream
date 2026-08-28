package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pgeske/filmstream/internal/recommendations"
)

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
