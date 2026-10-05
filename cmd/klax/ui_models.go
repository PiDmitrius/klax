package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/PiDmitrius/klax/internal/modelcatalog"
)

func (s *uiServer) handleModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiFail(w, http.StatusMethodNotAllowed, "method-not-allowed", "Метод не поддерживается")
		return
	}
	var body struct {
		Backend string `json:"backend"`
	}
	if err := decodeAPIRequest(http.MaxBytesReader(w, r.Body, 4096), &body, false); err != nil || (body.Backend != "claude" && body.Backend != "codex") {
		apiFail(w, http.StatusBadRequest, "bad-request", "Неизвестный движок")
		return
	}
	if s.d.models == nil {
		apiFail(w, http.StatusServiceUnavailable, "models-unavailable", "Каталог моделей недоступен")
		return
	}
	models, err := s.d.models.Refresh(r.Context(), body.Backend)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, modelcatalog.ErrUpdating) {
			code = http.StatusConflict
		}
		apiFail(w, code, "models-refresh-failed", "Не удалось обновить список моделей: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Backend string               `json:"backend"`
		Models  []modelcatalog.Model `json:"models"`
	}{body.Backend, models})
}
