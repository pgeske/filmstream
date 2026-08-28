package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/pgeske/filmstream/internal/metadata"
	"github.com/pgeske/filmstream/internal/recommendations"
)

const maxRecommendationPromptRequestBytes = 32 << 10

func (s *Server) listRecommendations(w http.ResponseWriter, _ *http.Request) {
	if s.recommendationService == nil {
		writeJSON(w, http.StatusOK, emptyRecommendationResponse())
		return
	}
	writeJSON(w, http.StatusOK, s.recommendationService.Refresh(recommendations.RefreshAutomatic))
}

func (s *Server) updateRecommendationPrompt(w http.ResponseWriter, r *http.Request) {
	if s.recommendationService == nil {
		writeError(w, http.StatusServiceUnavailable, "recommendations are not configured")
		return
	}
	defer r.Body.Close()
	var request struct {
		Prompt string `json:"prompt"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRecommendationPromptRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request: expected one JSON object")
		return
	}
	response, err := s.recommendationService.SetPrompt(request.Prompt)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, recommendations.ErrInvalidPrompt) || errors.Is(err, recommendations.ErrPromptTooLong) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) refreshRecommendations(w http.ResponseWriter, _ *http.Request) {
	if s.recommendationService == nil {
		writeJSON(w, http.StatusOK, emptyRecommendationResponse())
		return
	}
	writeJSON(w, http.StatusOK, s.recommendationService.Refresh(recommendations.RefreshManual))
}

func emptyRecommendationResponse() recommendations.Response {
	return recommendations.Response{Items: []metadata.Movie{}}
}
