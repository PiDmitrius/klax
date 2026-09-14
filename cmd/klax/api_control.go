package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/PiDmitrius/klax/internal/turnaudit"
)

// A turn publishes immutable boundary replies before closing their channels; HTTP never runs on the executor.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	status  int
}

func apiFailure(code string) *apiError {
	status, message := http.StatusInternalServerError, "Не удалось подготовить выполнение"
	switch code {
	case "read-only":
		status, message = http.StatusForbidden, "Доступ разрешён только для чтения"
	case "session-deleted":
		status, message = http.StatusNotFound, "Сессия удалена"
	case "session-not-found":
		status, message = http.StatusNotFound, "Сессия не найдена"
	case "result-unavailable":
		status, message = http.StatusConflict, "Результат этой границы недоступен"
	case "aborted":
		status, message = http.StatusConflict, "Сообщение удалено из очереди"
	case "audit-start-failed":
		message = "Стартовый гейт отклонил выполнение"
	case "attachments-missing":
		message = "Не удалось подготовить вложения"
	case "result-save-failed":
		message = "Не удалось сохранить результат хода"
	case "enqueue-failed":
		message = "Не удалось сохранить сообщение"
	}
	return &apiError{Code: code, Message: message, status: status}
}

func writeAPIError(w http.ResponseWriter, err *apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(err.status)
	_ = json.NewEncoder(w).Encode(struct {
		Error *apiError `json:"error"`
	}{err})
}

func optionalNonemptyString(raw json.RawMessage, field, fallback string) (string, *apiError) {
	if raw == nil {
		return fallback, nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || value == "" {
		return "", &apiError{Code: "invalid-" + strings.ReplaceAll(field, "_", "-"), Message: "Поле " + field + " должно быть непустой строкой", status: http.StatusBadRequest}
	}
	return value, nil
}

func (s *uiServer) requireSession(w http.ResponseWriter, sk string, created int64) bool {
	if s.d.store.Get(sk, created) == nil {
		writeAPIError(w, apiFailure("session-not-found"))
		return false
	}
	return true
}

type boundaryReply struct {
	event    *turnaudit.Event
	warnings []apiWarning
	err      *apiError
}
type apiWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type turnBoundary struct {
	done  chan struct{}
	reply boundaryReply
}
type turnWait struct {
	mu            sync.Mutex
	start, finish turnBoundary
}

func newTurnWait() *turnWait {
	return &turnWait{start: turnBoundary{done: make(chan struct{})}, finish: turnBoundary{done: make(chan struct{})}}
}
func (t *turnWait) publish(finish bool, reply boundaryReply) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	b := &t.start
	if finish {
		b = &t.finish
	}
	select {
	case <-b.done:
		return
	default:
	}
	b.reply = reply
	close(b.done)
}
func (t *turnWait) fail(code string) {
	if t == nil {
		return
	}
	reply := boundaryReply{err: apiFailure(code)}
	t.publish(false, reply)
	t.publish(true, reply)
}

type sendAdmission struct {
	completion *turnWait
	err        *apiError
}

func awaitTurn(w http.ResponseWriter, r *http.Request, t *turnWait, boundary string) {
	if t == nil {
		writeAPIError(w, apiFailure("result-unavailable"))
		return
	}
	b := &t.start
	if boundary == "finish" {
		b = &t.finish
	}
	select {
	case <-r.Context().Done():
		return
	case <-b.done:
	}
	if r.Context().Err() != nil {
		return
	}
	if b.reply.err != nil {
		writeAPIError(w, b.reply.err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		*turnaudit.Event
		Warnings []apiWarning `json:"warnings,omitempty"`
	}{b.reply.event, b.reply.warnings})
}

func decodeAPIRequest(body io.Reader, value any, allowEmpty bool) error {
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(value); err != nil {
		if allowEmpty && err == io.EOF {
			return nil
		}
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}

const retainedTurnResults = 64

func (sr *sessionRunner) pruneResultsLocked() {
	for len(sr.results) > retainedTurnResults {
		var oldest int64
		for seq, result := range sr.results {
			select {
			case <-result.finish.done:
				if oldest == 0 || seq < oldest {
					oldest = seq
				}
			default:
			}
		}
		if oldest == 0 {
			return
		}
		delete(sr.results, oldest)
	}
}
