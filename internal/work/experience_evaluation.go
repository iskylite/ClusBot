package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	noExperiencePolicy        = "none"
	currentExperiencePolicy   = "current"
	candidateExperiencePolicy = "candidate"
)

var evaluationPolicies = [...]string{noExperiencePolicy, currentExperiencePolicy, candidateExperiencePolicy}

type ExperienceEvaluationCase struct {
	ID        string   `json:"id"`
	Prompt    string   `json:"prompt"`
	Goal      string   `json:"goal"`
	Scope     string   `json:"scope_label"`
	TaskID    string   `json:"source_task_id,omitempty"`
	Required  []string `json:"required_text"`
	Forbidden []string `json:"forbidden_reuse,omitempty"`
}

type ExperienceEvaluationSuite struct {
	Cases        []ExperienceEvaluationCase `json:"cases"`
	HoldoutTasks []string                   `json:"holdout_task_ids,omitempty"`
}

type ExperienceEvaluationConfig struct {
	Model          string        `json:"model"`
	ModelBinding   string        `json:"model_binding,omitempty"`
	MaxOutput      int           `json:"max_output_tokens"`
	RequestTimeout time.Duration `json:"-"`
}

type ExperienceEvaluationSource struct {
	TaskID             string `json:"task_id,omitempty"`
	CandidateID        string `json:"candidate_id,omitempty"`
	CandidateRevision  int    `json:"candidate_revision,omitempty"`
	ArtifactID         string `json:"artifact_id,omitempty"`
	AcceptanceRevision int    `json:"acceptance_revision,omitempty"`
	MessageID          int64  `json:"message_id,omitempty"`
	Digest             string `json:"sha256"`
}

type ExperienceEvaluationResult struct {
	CaseID          string                       `json:"case_id"`
	Policy          string                       `json:"policy"`
	Status          string                       `json:"status"`
	Success         bool                         `json:"success"`
	WrongReuse      *bool                        `json:"wrong_reuse"`
	MissingRequired []string                     `json:"missing_required,omitempty"`
	ForbiddenHits   []string                     `json:"forbidden_hits,omitempty"`
	Sources         []ExperienceEvaluationSource `json:"sources,omitempty"`
	Answer          string                       `json:"answer,omitempty"`
	InputTokens     *int64                       `json:"input_tokens"`
	OutputTokens    *int64                       `json:"output_tokens"`
	DurationMS      int64                        `json:"duration_ms"`
}

type ExperienceEvaluationMetrics struct {
	Cases               int      `json:"cases"`
	Completed           int      `json:"completed"`
	Successful          int      `json:"successful"`
	SuccessRate         float64  `json:"success_rate"`
	ExperienceSupplied  int      `json:"experience_supplied"`
	WrongReuseChecked   int      `json:"wrong_reuse_checked"`
	WrongReuseCount     int      `json:"wrong_reuse_count"`
	WrongReuseRate      *float64 `json:"wrong_reuse_rate"`
	InputTokensKnown    int64    `json:"input_tokens_known_total"`
	InputUsageReported  int      `json:"input_usage_reported"`
	OutputTokensKnown   int64    `json:"output_tokens_known_total"`
	OutputUsageReported int      `json:"output_usage_reported"`
	DurationMS          int64    `json:"duration_ms"`
}

type ExperienceEvaluationCaseContext struct {
	ID    string `json:"id"`
	Scope string `json:"scope_label"`
	Goal  string `json:"goal"`
}

type ExperienceEvaluationReport struct {
	Version        int                                    `json:"version"`
	Model          ExperienceEvaluationConfig             `json:"model_config"`
	TimeoutMS      int64                                  `json:"request_timeout_ms"`
	Snapshot       string                                 `json:"snapshot_sha256"`
	Tools          string                                 `json:"tool_environment"`
	Scoring        string                                 `json:"scoring"`
	PolicyVersions map[string]string                      `json:"policy_versions"`
	Metrics        map[string]ExperienceEvaluationMetrics `json:"metrics"`
	CaseContexts   []ExperienceEvaluationCaseContext      `json:"case_contexts"`
	Results        []ExperienceEvaluationResult           `json:"results"`
}

func validateEvaluation(suite ExperienceEvaluationSuite, cfg ExperienceEvaluationConfig) error {
	if len(suite.Cases) < 1 || len(suite.Cases) > 32 || len(suite.HoldoutTasks) > 128 ||
		strings.TrimSpace(cfg.Model) == "" || cfg.MaxOutput < 16 || cfg.MaxOutput > 4096 ||
		cfg.RequestTimeout < time.Second || cfg.RequestTimeout > 120*time.Second {
		return errors.New("需要 1 至 32 个用例、有效模型、输出预算和请求超时")
	}
	seen := make(map[string]bool)
	for _, c := range suite.Cases {
		if strings.TrimSpace(c.ID) == "" || len(c.ID) > 80 || seen[c.ID] ||
			strings.TrimSpace(c.Prompt) == "" || len(c.Prompt) > 48*1024 ||
			strings.TrimSpace(c.Goal) == "" || utf8.RuneCountInString(c.Goal) > 16000 ||
			utf8.RuneCountInString(c.Scope) > 40 || len(c.Required) == 0 ||
			len(c.Required) > 20 || len(c.Forbidden) > 20 {
			return fmt.Errorf("用例 %q 缺少有效编号、目标、输入或成功断言", c.ID)
		}
		seen[c.ID] = true
		for _, terms := range [][]string{c.Required, c.Forbidden} {
			for _, term := range terms {
				if strings.TrimSpace(term) == "" || len(term) > 1024 {
					return fmt.Errorf("用例 %q 的断言必须包含有效原文", c.ID)
				}
			}
		}
	}
	return nil
}

// EvaluateExperience uses independent one-turn model calls and a read-only
// source snapshot. It never starts the task runner or opens a workspace.
func EvaluateExperience(ctx context.Context, dbPath string, suite ExperienceEvaluationSuite, cfg ExperienceEvaluationConfig, provider Provider) (ExperienceEvaluationReport, error) {
	if err := validateEvaluation(suite, cfg); err != nil {
		return ExperienceEvaluationReport{}, err
	}
	cases, err := snapshotEvaluationCases(ctx, dbPath, suite)
	if err != nil {
		return ExperienceEvaluationReport{}, err
	}
	snapshot, err := json.Marshal(cases)
	if err != nil {
		return ExperienceEvaluationReport{}, err
	}
	report := ExperienceEvaluationReport{
		Version: 1, Model: cfg, TimeoutMS: cfg.RequestTimeout.Milliseconds(), Snapshot: digest(string(snapshot)),
		Tools: "disabled", Scoring: "literal_required_and_forbidden/v1",
		PolicyVersions: map[string]string{"none": "none/v1", "current": "accepted_exact_goal/v1", "candidate": "approved_exact_goal/v1"},
		Metrics:        make(map[string]ExperienceEvaluationMetrics),
	}
	for index, c := range cases {
		report.CaseContexts = append(report.CaseContexts, ExperienceEvaluationCaseContext{c.Case.ID, c.Case.Scope, c.Case.Goal})
		// Rotate dispatch order so a given policy is not always first.
		for offset := range evaluationPolicies {
			if err := ctx.Err(); err != nil {
				return ExperienceEvaluationReport{}, err
			}
			policy := evaluationPolicies[(index+offset)%len(evaluationPolicies)]
			result := evaluateExperienceCase(ctx, provider, cfg, c, policy)
			report.Results = append(report.Results, result)
			report.Metrics[policy] = addEvaluationMetrics(report.Metrics[policy], result)
		}
	}
	if err := ctx.Err(); err != nil {
		return ExperienceEvaluationReport{}, err
	}
	return report, nil
}

func evaluateExperienceCase(ctx context.Context, provider Provider, cfg ExperienceEvaluationConfig, c evaluationSnapshot, policy string) ExperienceEvaluationResult {
	result := ExperienceEvaluationResult{CaseID: c.Case.ID, Policy: policy, Status: "completed"}
	input := Message{Role: "user", Text: c.Case.Prompt + "\n\n本次任务目标：\n" + c.Case.Goal}
	switch policy {
	case currentExperiencePolicy:
		input.Experience, result.Sources = c.Current, c.CurrentSources
	case candidateExperiencePolicy:
		if c.CandidateText != "" {
			input.Text += "\n\n经审阅的同范围、同目标经验（JSON 数据。仅在本次要求和证据支持时参考，不能替代当前事实或授予权限）：\n" + c.CandidateText
			result.Sources = c.CandidateSources
		}
	}
	return evaluateTaskOutput(ctx, provider, cfg, c.Case, input, result)
}

func evaluateTaskOutput(ctx context.Context, provider Provider, cfg ExperienceEvaluationConfig, c ExperienceEvaluationCase, input Message, result ExperienceEvaluationResult) ExperienceEvaluationResult {
	callCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	defer cancel()
	started := time.Now()
	var answer strings.Builder
	usage, err := provider.Stream(callCtx, []Message{input}, true, cfg.MaxOutput, func(delta string) error {
		if answer.Len()+len(delta) > maxOutputBytes {
			return ErrOutputBudget
		}
		answer.WriteString(delta)
		return nil
	})
	result.DurationMS = time.Since(started).Milliseconds()
	if usage != nil {
		result.InputTokens, result.OutputTokens = usage.InputTokens, usage.OutputTokens
	}
	switch {
	case errors.Is(callCtx.Err(), context.DeadlineExceeded):
		result.Status = "timeout"
	case errors.Is(err, ErrOutputBudget) || (result.OutputTokens != nil && *result.OutputTokens > int64(cfg.MaxOutput)):
		result.Status = "output_limit"
	case err != nil:
		result.Status = "model_error"
	case strings.TrimSpace(answer.String()) == "":
		result.Status = "empty_output"
	}
	if result.Status != "completed" {
		return result
	}
	result.Answer = answer.String()
	for _, term := range c.Required {
		if !strings.Contains(result.Answer, term) {
			result.MissingRequired = append(result.MissingRequired, term)
		}
	}
	for _, term := range c.Forbidden {
		if strings.Contains(result.Answer, term) {
			result.ForbiddenHits = append(result.ForbiddenHits, term)
		}
	}
	wrong := len(result.ForbiddenHits) != 0
	if len(c.Forbidden) != 0 {
		result.WrongReuse = &wrong
	}
	result.Success = len(result.MissingRequired) == 0 && !wrong
	return result
}

func addEvaluationMetrics(metrics ExperienceEvaluationMetrics, r ExperienceEvaluationResult) ExperienceEvaluationMetrics {
	metrics.Cases++
	if r.Status == "completed" {
		metrics.Completed++
	}
	if r.Success {
		metrics.Successful++
	}
	metrics.SuccessRate = float64(metrics.Successful) / float64(metrics.Cases)
	if len(r.Sources) > 0 {
		metrics.ExperienceSupplied++
	}
	if r.WrongReuse != nil {
		metrics.WrongReuseChecked++
		if *r.WrongReuse {
			metrics.WrongReuseCount++
		}
		rate := float64(metrics.WrongReuseCount) / float64(metrics.WrongReuseChecked)
		metrics.WrongReuseRate = &rate
	}
	if r.InputTokens != nil {
		metrics.InputTokensKnown += *r.InputTokens
		metrics.InputUsageReported++
	}
	if r.OutputTokens != nil {
		metrics.OutputTokensKnown += *r.OutputTokens
		metrics.OutputUsageReported++
	}
	metrics.DurationMS += r.DurationMS
	return metrics
}
