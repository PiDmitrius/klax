package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/PiDmitrius/klax/internal/modelcatalog"
)

func (s *uiServer) handleModelsRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Backend string `json:"backend"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || (body.Backend != "claude" && body.Backend != "codex") {
		http.Error(w, "Неизвестный движок", http.StatusBadRequest)
		return
	}
	if s.d.models == nil {
		http.Error(w, "Каталог моделей недоступен", http.StatusServiceUnavailable)
		return
	}
	models, err := s.d.models.Refresh(r.Context(), body.Backend)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, modelcatalog.ErrUpdating) {
			code = http.StatusConflict
		}
		http.Error(w, "Не удалось обновить список моделей: "+err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Backend string               `json:"backend"`
		Models  []modelcatalog.Model `json:"models"`
	}{body.Backend, models})
}
