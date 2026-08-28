package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/pgeske/filmstream/internal/config"
)

type fakeModel struct {
	content string
	calls   int
}

func (f *fakeModel) Complete(_ context.Context, _, _ string) (string, error) {
	f.calls++
	return f.content, nil
}

func TestSemanticResolverValidatesAndSortsCandidates(t *testing.T) {
	model := &fakeModel{content: "```json\n{\"candidates\":[{\"title\":\"Possibility\",\"year\":1999,\"confidence\":0.3},{\"title\":\"The Movie\",\"year\":2001,\"confidence\":1.2}]}\n```"}
	configured := NewSemantic(model, "test", "small")
	result, err := configured.Resolve(context.Background(), "rough description")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 2 || result.Candidates[0].Title != "The Movie" || result.Candidates[0].Confidence != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestOpenAICompatibleJSONMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["response_format"] == nil {
			t.Error("response_format was not sent")
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"{\"candidates\":[]}"}}]}`)
	}))
	defer server.Close()
	model, err := NewOpenAICompatible(server.URL+"/v1", "small", "secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	content, err := model.Complete(context.Background(), "system", "user")
	if err != nil {
		t.Fatal(err)
	}
	if content != `{"candidates":[]}` {
		t.Fatalf("content = %q", content)
	}
}

func TestModelFromConfigUsesResolverCredentialsWithModelOverride(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "openai-api-key")
	if err := os.WriteFile(keyPath, []byte("shared-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	model, err := ModelFromConfig(config.Resolver{
		Provider: "openai-compatible", BaseURL: "https://example.test/v1", Model: "resolver-model",
		APIKeyFile: keyPath,
	}, "recommendation-model")
	if err != nil {
		t.Fatal(err)
	}
	configured, ok := model.(*OpenAICompatible)
	if !ok {
		t.Fatalf("model type = %T", model)
	}
	if configured.model != "recommendation-model" || configured.apiKey != "shared-secret" {
		t.Fatalf("configured model = %q, key = %q", configured.model, configured.apiKey)
	}
}

func TestCachedResolverPersistsResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	model := &fakeModel{content: `{"candidates":[{"title":"The Movie","year":2001,"confidence":0.9}]}`}
	first := NewCached(NewSemantic(model, "test", "small"), path, "test/small")
	if _, err := first.Resolve(context.Background(), "that movie"); err != nil {
		t.Fatal(err)
	}
	second := NewCached(NewSemantic(model, "test", "small"), path, "test/small")
	result, err := second.Resolve(context.Background(), "that   movie")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Cached || model.calls != 1 {
		t.Fatalf("cached = %v, calls = %d", result.Cached, model.calls)
	}
}
