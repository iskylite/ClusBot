package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"clusbot/internal/work"
)

var ErrModelDiscovery = errors.New("获取模型列表失败")

type DiscoveredModel struct {
	ID         string               `json:"id"`
	Attributes work.ModelAttributes `json:"attributes"`
}

func (s *Settings) Discover(ctx context.Context, draft SettingsInput) ([]DiscoveredModel, error) {
	stored, err := s.Store.ModelSettings(ctx)
	if err != nil {
		return nil, err
	}
	value := s.effective(settingsWithKey(stored, draft))
	value.ModelID, value.Attributes = "", nil
	if value.Protocol == "" {
		value.Protocol = stored.Protocol
	}
	if err := validateSettings(value); err != nil {
		return nil, err
	}
	return discoverModelValues(ctx, value, keyValue(value))
}

func discoverModelValues(ctx context.Context, value work.ModelSettings, key string) ([]DiscoveredModel, error) {
	endpoint, err := apiEndpoint(value.BaseURL, "/models")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, ErrModelDiscovery
	}
	request.Header.Set("Accept", "application/json")
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, ErrModelDiscovery
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w（HTTP %d）", ErrModelDiscovery, response.StatusCode)
	}
	const limit = 4 << 20
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(raw) > limit {
		return nil, ErrModelDiscovery
	}
	var catalog struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
			Architecture  struct {
				InputModalities []string `json:"input_modalities"`
			} `json:"architecture"`
			TopProvider struct {
				MaxCompletionTokens int `json:"max_completion_tokens"`
			} `json:"top_provider"`
			MaxOutputTokens     int      `json:"max_output_tokens"`
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &catalog) != nil || catalog.Data == nil {
		return nil, ErrModelDiscovery
	}
	models := make([]DiscoveredModel, 0, len(catalog.Data))
	seen := map[string]bool{}
	for _, entry := range catalog.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" || len(id) > 256 || seen[id] {
			continue
		}
		seen[id] = true
		item := DiscoveredModel{ID: id}
		if entry.ContextLength > 0 {
			item.Attributes.ContextTokens = &entry.ContextLength
		}
		if output := entry.TopProvider.MaxCompletionTokens; output > 0 {
			item.Attributes.MaxOutputTokens = &output
		} else if entry.MaxOutputTokens > 0 {
			output := entry.MaxOutputTokens
			item.Attributes.MaxOutputTokens = &output
		}
		if len(entry.Architecture.InputModalities) > 0 {
			vision := slices.Contains(entry.Architecture.InputModalities, "image")
			item.Attributes.Vision = &vision
		}
		if slices.Contains(entry.SupportedParameters, "reasoning") || slices.Contains(entry.SupportedParameters, "reasoning_effort") {
			supported := true
			item.Attributes.Reasoning = &supported
		}
		models = append(models, item)
	}
	slices.SortFunc(models, func(a, b DiscoveredModel) int { return strings.Compare(a.ID, b.ID) })
	return models, nil
}
