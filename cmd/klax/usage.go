package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PiDmitrius/klax/internal/fmtutil"
)

type claudeUsageWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type claudeUsageResp struct {
	FiveHour *claudeUsageWindow `json:"five_hour"`
	SevenDay *claudeUsageWindow `json:"seven_day"`
}

func fetchClaudeUsage() (*claudeUsageResp, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	token := creds.ClaudeAiOauth.AccessToken
	if token == "" {
		return nil, errors.New("OAuth токен не найден — авторизуйся в claude CLI")
	}

	req, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com/api/oauth/usage", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("OAuth токен просрочен — запусти claude CLI чтобы рефрешнуть")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var u claudeUsageResp
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &u, nil
}

func formatUsageWindow(label string, w *claudeUsageWindow) string {
	if w == nil {
		return fmt.Sprintf("%s: —", label)
	}
	pct := int(w.Utilization)
	t, err := time.Parse(time.RFC3339Nano, w.ResetsAt)
	if err != nil {
		return fmt.Sprintf("%s: <b>%d%%</b>", label, pct)
	}
	in := time.Until(t)
	if in <= 0 {
		return fmt.Sprintf("%s: <b>%d%%</b>", label, pct)
	}
	return fmt.Sprintf("%s: <b>%d%%</b> (сброс через <b>%s</b>)", label, pct, fmtutil.Duration(in))
}

func formatClaudeUsage(u *claudeUsageResp) string {
	var lines []string
	lines = append(lines, "📊 Usage (Claude)")
	lines = append(lines, formatUsageWindow("⏱ 5ч", u.FiveHour))
	lines = append(lines, formatUsageWindow("📅 7д", u.SevenDay))
	return strings.Join(lines, "\n")
}

func (d *daemon) handleUsage(chatID, msgID, sk string) {
	sess := d.store.Active(sk)
	if sess == nil {
		d.sendMessage(chatID, msgID, "Нет активной сессии")
		return
	}
	backend := effectiveBackendName(d.cfg, d.scopeDefaults(sk), sess)
	if backend != "claude" {
		d.sendMessage(chatID, msgID, "/usage доступна только для backend claude.")
		return
	}
	go func() {
		t, _, fmtStr := d.transportFor(chatID)
		if t == nil {
			return
		}
		ctx, cancel := withDeliveryTimeout(context.Background())
		defer cancel()
		chain, err := d.syncMessageChain(ctx, chatID, msgID, nil, "...", fmtStr)
		if err != nil {
			return
		}
		u, fetchErr := fetchClaudeUsage()
		if fetchErr != nil {
			_, _ = d.syncMessageChain(ctx, chatID, msgID, chain, fmt.Sprintf("❌ %v", fetchErr), fmtStr)
			return
		}
		_, _ = d.syncMessageChain(ctx, chatID, msgID, chain, formatClaudeUsage(u), fmtStr)
	}()
}
