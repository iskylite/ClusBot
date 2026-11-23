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
	Name       string               `json:"name,omitempty"`
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
		Data []json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &catalog) != nil || catalog.Data == nil {
		return nil, ErrModelDiscovery
	}
	models := make([]DiscoveredModel, 0, len(catalog.Data))
	seen := map[string]bool{}
	for _, rawEntry := range catalog.Data {
		var entry map[string]json.RawMessage
		if json.Unmarshal(rawEntry, &entry) != nil {
			continue
		}
		var id string
		_ = json.Unmarshal(entry["id"], &id)
		id = strings.TrimSpace(id)
		if id == "" || len(id) > 256 || seen[id] {
			continue
		}
		seen[id] = true
		item := DiscoveredModel{ID: id}
		var name string
		if json.Unmarshal(entry["name"], &name) == nil && validCatalogName(strings.TrimSpace(name)) {
			item.Name = strings.TrimSpace(name)
		}
		item.Attributes = discoveredAttributes(entry)
		models = append(models, item)
	}
	slices.SortFunc(models, func(a, b DiscoveredModel) int { return strings.Compare(a.ID, b.ID) })
	return models, nil
}

// Read only documented fields with known meanings; arbitrary model names and
// unrecognised metadata are not capability evidence. Invalid optional fields
// are ignored independently rather than invalidating the entire catalog.
func discoveredAttributes(entry map[string]json.RawMessage) work.ModelAttributes {
	a := work.ModelAttributes{Sources: map[string]string{}}
	source := func(key string) { a.Sources[key] = "provider" }
	intField := func(object map[string]json.RawMessage, key string) *int {
		var value int
		if json.Unmarshal(object[key], &value) == nil && value > 0 {
			return &value
		}
		return nil
	}
	boolField := func(object map[string]json.RawMessage, key string) *bool {
		var value *bool
		if json.Unmarshal(object[key], &value) == nil {
			return value
		}
		return nil
	}
	stringsField := func(object map[string]json.RawMessage, key string) []string {
		var values []string
		if json.Unmarshal(object[key], &values) != nil || len(values) == 0 {
			return nil
		}
		return values
	}
	objectField := func(key string) map[string]json.RawMessage {
		var value map[string]json.RawMessage
		_ = json.Unmarshal(entry[key], &value)
		return value
	}
	a.ContextTokens = intField(entry, "context_length")
	if a.ContextTokens == nil {
		a.ContextTokens = intField(entry, "context_window_tokens")
	}
	if a.ContextTokens != nil {
		source("context_tokens")
	}
	a.MaxOutputTokens = intField(objectField("top_provider"), "max_completion_tokens")
	if a.MaxOutputTokens == nil {
		a.MaxOutputTokens = intField(entry, "max_output_tokens")
	}
	if a.MaxOutputTokens == nil {
		a.MaxOutputTokens = intField(entry, "maximum_output_tokens")
	}
	if a.MaxOutputTokens != nil {
		source("max_output_tokens")
	}

	// OpenRouter's input_modalities is a declared list of accepted inputs.
	if modalities := stringsField(objectField("architecture"), "input_modalities"); len(modalities) > 0 {
		known := true
		for _, modality := range modalities {
			if !slices.Contains([]string{"text", "image", "file", "audio", "video"}, modality) {
				known = false
			}
		}
		vision, file := slices.Contains(modalities, "image"), slices.Contains(modalities, "file")
		if vision || known {
			a.Vision = &vision
			source("vision")
		}
		if file || known {
			a.FileInput = &file
			source("file_input")
		}
	}
	if value := boolField(entry, "supports_vision"); value != nil {
		a.Vision = value
		source("vision")
	}
	if value := boolField(entry, "supports_tools"); value != nil {
		a.ToolCalling = value
		source("tool_calling")
	}
	if value := boolField(entry, "supports_structured_output"); value != nil {
		a.StructuredOutput = value
		source("structured_output")
	}
	if value := boolField(entry, "supports_reasoning"); value != nil {
		a.Reasoning = value
		source("reasoning")
	}
	if params := stringsField(entry, "supported_parameters"); len(params) > 0 {
		yes := true
		if a.Reasoning == nil && (slices.Contains(params, "reasoning") || slices.Contains(params, "reasoning_effort")) {
			a.Reasoning = &yes
			source("reasoning")
		}
		if a.ToolCalling == nil && slices.Contains(params, "tools") {
			a.ToolCalling = &yes
			source("tool_calling")
		}
		if a.StructuredOutput == nil && slices.Contains(params, "structured_outputs") {
			a.StructuredOutput = &yes
			source("structured_output")
		}
	}
	levels := stringsField(objectField("reasoning"), "supported_efforts")
	if len(levels) == 0 {
		levels = stringsField(entry, "supported_reasoning_efforts")
	}
	if len(levels) > 0 && len(levels) <= 16 && (a.Reasoning == nil || *a.Reasoning) {
		valid := true
		for _, level := range levels {
			if level == "" || len(level) > 32 || strings.TrimSpace(level) != level {
				valid = false
				break
			}
		}
		if valid {
			for _, level := range levels {
				if !slices.Contains(a.ReasoningLevels, level) {
					a.ReasoningLevels = append(a.ReasoningLevels, level)
				}
			}
			source("reasoning_levels")
			if a.Reasoning == nil {
				yes := true
				a.Reasoning = &yes
				source("reasoning")
			}
		}
	}
	if len(a.Sources) == 0 {
		a.Sources = nil
	}
	return a
}
