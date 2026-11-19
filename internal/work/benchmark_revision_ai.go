package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

var ErrBenchmarkRevisionModel = errors.New("未取得模型的有效修订建议")
var ErrBenchmarkRevisionModelBinding = errors.New("专用基准修订模型档案已不可用，请检查绑定、版本或凭据")

type BenchmarkRevisionAIInput struct {
	BenchmarkRevisionProposalInput
	AllowExpectedActualChange bool `json:"allow_expected_actual_change,omitempty"`
}

type BenchmarkRevisionModelInput struct {
	Text  string                       `json:"text"`
	Cases []BenchmarkRevisionModelCase `json:"editable_cases"`
}

type BenchmarkRevisionModelCase struct {
	Key        string                            `json:"case_key"`
	Parameters []BenchmarkRevisionModelParameter `json:"parameters"`
}

type BenchmarkRevisionModelParameter struct {
	Key             string `json:"parameter"`
	HasActualCheck  bool   `json:"has_expected_actual_check"`
	MayChangeActual bool   `json:"may_change_expected_actual_check"`
}

type BenchmarkRevisionModel func(context.Context, BenchmarkRevisionModelInput, int) (string, error)

func (s *Store) ProposeBenchmarkRevisionAI(ctx context.Context, actor, task string, in BenchmarkRevisionAIInput, provider BenchmarkProvider, interpret BenchmarkRevisionModel) (BenchmarkRevisionProposal, error) {
	if strings.TrimSpace(in.Text) == "" || len(in.Text) > 4096 {
		return BenchmarkRevisionProposal{}, ErrBenchmarkInput
	}
	current, err := s.benchmarkRevisionProposalBase(ctx, actor, task, in.ExpectedRevision)
	if err != nil {
		return BenchmarkRevisionProposal{}, err
	}
	if interpret == nil {
		return BenchmarkRevisionProposal{}, ErrBenchmarkRevisionModel
	}
	// Check actor, version and the mutable subset again before sharing even
	// redacted identifiers. The validator rechecks after the model returns.
	rows, err := s.db.QueryContext(ctx, `SELECT c.case_key FROM benchmark_cases c
JOIN benchmark_runs r ON r.task_id=c.task_id JOIN task_states t ON t.task_id=r.task_id
WHERE r.task_id=? AND r.actor=? AND r.plan_revision=? AND r.stop_requested=0
AND t.state IN ('pending','running','paused') AND c.phase='queued' AND c.operation_id=''`, task, actor, current.Revision)
	if err != nil {
		return BenchmarkRevisionProposal{}, err
	}
	editable := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return BenchmarkRevisionProposal{}, err
		}
		editable[key] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return BenchmarkRevisionProposal{}, err
	}
	if len(editable) == 0 {
		return BenchmarkRevisionProposal{}, fmt.Errorf("%w：没有可修改的待执行测项，请检查任务状态和方案版本", ErrState)
	}
	request := BenchmarkRevisionModelInput{Text: in.Text}
	for _, c := range current.Profile.Cases {
		if !editable[c.Key] {
			continue
		}
		item := BenchmarkRevisionModelCase{Key: c.Key, Parameters: []BenchmarkRevisionModelParameter{}}
		for key := range c.Parameters {
			_, checked := c.ActualConfig[key]
			item.Parameters = append(item.Parameters, BenchmarkRevisionModelParameter{key, checked, checked && in.AllowExpectedActualChange})
		}
		slices.SortFunc(item.Parameters, func(a, b BenchmarkRevisionModelParameter) int { return strings.Compare(a.Key, b.Key) })
		request.Cases = append(request.Cases, item)
	}
	requestBytes, err := json.Marshal(request)
	if err != nil || len(requestBytes) > 24*1024 {
		return BenchmarkRevisionProposal{}, fmt.Errorf("%w：可修改的测项字段太多，不能生成单步建议", ErrBenchmarkInput)
	}
	call, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	release, err := s.acquireModelSlot(call)
	if err != nil {
		return BenchmarkRevisionProposal{}, ErrBenchmarkRevisionModel
	}
	text, err := func() (string, error) {
		defer release()
		s.runtimeMu.Lock()
		budget := min(512, s.admissionLimits.OutputTokens)
		s.runtimeMu.Unlock()
		if budget < 32 {
			return "", ErrBenchmarkRevisionModel
		}
		return interpret(call, request, budget)
	}()
	if errors.Is(err, ErrBenchmarkRevisionModelBinding) {
		return BenchmarkRevisionProposal{}, err
	}
	if err != nil || call.Err() != nil {
		return BenchmarkRevisionProposal{}, ErrBenchmarkRevisionModel
	}
	intent, err := parseBenchmarkRevisionModelOutput(text, in)
	if err != nil {
		return BenchmarkRevisionProposal{}, err
	}
	if !editable[intent.CaseKey] {
		return BenchmarkRevisionProposal{}, fmt.Errorf("%w：模型所指测项不在本次可修改范围", ErrBenchmarkInput)
	}
	return s.previewBenchmarkRevisionIntent(ctx, actor, task, current, intent, provider, "model_interpreted")
}

// A single flat object, exact keys and no duplicate keys. No arbitrary text,
// tool input, lists of candidates, extra output or embedded patches are read.
func parseBenchmarkRevisionModelOutput(raw string, input BenchmarkRevisionAIInput) (BenchmarkRevisionIntent, error) {
	var intent BenchmarkRevisionIntent
	if len(raw) > 4096 {
		return intent, ErrBenchmarkRevisionModel
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	if open, err := dec.Token(); err != nil || open != json.Delim('{') {
		return intent, ErrBenchmarkRevisionModel
	}
	seen := map[string]bool{}
	var quoted string
	for dec.More() {
		t, err := dec.Token()
		key, ok := t.(string)
		if err != nil || !ok || seen[key] {
			return intent, ErrBenchmarkRevisionModel
		}
		seen[key] = true
		switch key {
		case "action":
			err = dec.Decode(&intent.Action)
		case "case_key":
			err = dec.Decode(&intent.CaseKey)
		case "parameter":
			err = dec.Decode(&intent.Parameter)
		case "value":
			err = dec.Decode(&intent.Value)
		case "change_expected_actual_config":
			var value *bool
			err = dec.Decode(&value)
			if value == nil {
				return intent, ErrBenchmarkRevisionModel
			}
			intent.ChangeActual = *value
		case "quote":
			err = dec.Decode(&quoted)
		default:
			return intent, ErrBenchmarkRevisionModel
		}
		if err != nil {
			return intent, ErrBenchmarkRevisionModel
		}
	}
	if close, err := dec.Token(); err != nil || close != json.Delim('}') {
		return intent, ErrBenchmarkRevisionModel
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return intent, ErrBenchmarkRevisionModel
	}
	if intent.Action == "clarify" && len(seen) == 1 {
		return intent, fmt.Errorf("%w：请明确一个待执行测项和一处要修改的内容", ErrBenchmarkInput)
	}
	if !validBenchKey(intent.CaseKey) || quoted == "" || strings.TrimSpace(quoted) != quoted || !strings.Contains(input.Text, quoted) {
		return intent, ErrBenchmarkRevisionModel
	}
	switch intent.Action {
	case "remove_case":
		if len(seen) != 3 || !seen["action"] || !seen["case_key"] || !seen["quote"] {
			return intent, ErrBenchmarkRevisionModel
		}
	case "set_parameter":
		if len(seen) != 6 || !validBenchKey(intent.Parameter) || intent.Value == "" || len(intent.Value) > 256 ||
			strings.TrimSpace(intent.Value) != intent.Value || strings.ContainsAny(intent.Value, "\r\n\x00") {
			return intent, ErrBenchmarkRevisionModel
		}
		if !benchmarkValueInQuote(quoted, intent.Value) {
			return intent, ErrBenchmarkRevisionModel
		}
		if intent.ChangeActual && !input.AllowExpectedActualChange {
			return intent, fmt.Errorf("%w：请显式确认是否连同实测要求一起修订", ErrBenchmarkInput)
		}
	default:
		return intent, ErrBenchmarkRevisionModel
	}
	return intent, nil
}

// Bind proposed values to an exact, whole token in the user quote. For
// example, a proposal for 2 must not claim the quoted value 32 as evidence.
func benchmarkValueInQuote(quote, value string) bool {
	token := func(b byte) bool {
		return b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || strings.ContainsRune("_.+%-", rune(b))
	}
	for start := 0; start < len(quote); {
		n := strings.Index(quote[start:], value)
		if n < 0 {
			return false
		}
		left := start + n
		right := left + len(value)
		if (left == 0 || !token(quote[left-1])) && (right == len(quote) || !token(quote[right])) {
			return true
		}
		start = left + 1
	}
	return false
}
