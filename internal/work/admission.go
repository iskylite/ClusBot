package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrClassification            = errors.New("委托分类未完成")
	ErrClassificationUnconfirmed = fmt.Errorf("%w：结果尚未确认", ErrClassification)
)

type DelegationResult struct {
	Task        bool
	Route       *RouteDecision
	Usage       *TokenUsage
	Requests    int
	RouterModel *RouterModelProvenance
}

type DelegationClassifier func(context.Context, string, []Message, int) (DelegationResult, error)

type submissionClassification struct {
	DelegationResult
	activeMS   int64
	outputUsed int
}

type admissionFlight struct {
	fingerprint string
	done        chan struct{}
	result      submissionClassification
	err         error
}

// Runtime limits also apply when submissions arrive before Start. Start uses
// the configured values; direct Store callers share the same conservative gate.
func (s *Store) configureRuntime(concurrency int, limits RunLimits) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	s.modelConcurrency = max(1, concurrency)
	s.admissionLimits = limits
	if s.modelAvailable != nil {
		close(s.modelAvailable)
		s.modelAvailable = nil
	}
}

func (s *Store) acquireModelSlot(ctx context.Context) (func(), error) {
	for {
		s.runtimeMu.Lock()
		if s.modelConcurrency == 0 {
			s.modelConcurrency = 2
			s.admissionLimits = RunLimits{ActiveTime: 10 * time.Minute, ModelTimeout: 2 * time.Minute, OutputTokens: 4096}
		}
		if s.modelActive < s.modelConcurrency {
			if err := ctx.Err(); err != nil {
				s.runtimeMu.Unlock()
				return nil, err
			}
			s.modelActive++
			s.runtimeMu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					s.runtimeMu.Lock()
					defer s.runtimeMu.Unlock()
					s.modelActive--
					if s.modelAvailable != nil {
						close(s.modelAvailable)
						s.modelAvailable = nil
					}
				})
			}, nil
		}
		if s.modelAvailable == nil {
			s.modelAvailable = make(chan struct{})
		}
		available := s.modelAvailable
		s.runtimeMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-available:
		}
	}
}

func (s *Store) classifySubmission(ctx context.Context, key, fingerprint, conversationID, text string, history []Message, classify DelegationClassifier) (submissionClassification, error) {
	s.admissionMu.Lock()
	if flight := s.admissions[key]; flight != nil {
		s.admissionMu.Unlock()
		if flight.fingerprint != fingerprint {
			return submissionClassification{}, ErrConflict
		}
		select {
		case <-ctx.Done():
			return submissionClassification{}, ctx.Err()
		case <-flight.done:
			return flight.result, flight.err
		}
	}
	flight := &admissionFlight{fingerprint: fingerprint, done: make(chan struct{})}
	if s.admissions == nil {
		s.admissions = make(map[string]*admissionFlight)
	}
	s.admissions[key] = flight
	s.admissionMu.Unlock()
	defer func() {
		s.admissionMu.Lock()
		delete(s.admissions, key)
		close(flight.done)
		s.admissionMu.Unlock()
	}()
	flight.result, flight.err = s.classifyOnce(ctx, key, fingerprint, conversationID, text, history, classify)
	return flight.result, flight.err
}

func (s *Store) classifyOnce(ctx context.Context, key, fingerprint, conversationID, text string, history []Message, classify DelegationClassifier) (submissionClassification, error) {
	var result submissionClassification
	var savedFingerprint, state, failure, route, model string
	var usage TokenUsage
	err := s.db.QueryRowContext(ctx, `SELECT fingerprint,state,is_task,requests,input_tokens,output_tokens,active_ms,output_used,error,route,router_model_provenance
FROM submission_classifications WHERE request_key=?`, key).Scan(&savedFingerprint, &state, &result.Task, &result.Requests,
		&usage.InputTokens, &usage.OutputTokens, &result.activeMS, &result.outputUsed, &failure, &route, &model)
	if err == nil {
		if savedFingerprint != fingerprint {
			return result, ErrConflict
		}
		if usage.InputTokens != nil || usage.OutputTokens != nil {
			result.Usage = &usage
		}
		result.RouterModel, err = decodeRouterModelProvenance(model)
		if err != nil {
			return result, err
		}
		if state == "completed" {
			if route != "" {
				if err := json.Unmarshal([]byte(route), &result.Route); err != nil {
					return result, err
				}
			}
			return result, nil
		}
		if state == "running" {
			return result, fmt.Errorf("%w；上次分类已中断，不会自动重复请求，可明确选择重新判断并发送", ErrClassificationUnconfirmed)
		}
		return result, fmt.Errorf("%w：%s", ErrClassification, failure)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	// Acquire outside any transaction so classification cannot monopolize the
	// single SQLite connection while waiting for capacity or the provider.
	release, err := s.acquireModelSlot(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	s.runtimeMu.Lock()
	limits := s.admissionLimits
	s.runtimeMu.Unlock()
	maxRoutingOutput := 256
	if routed, _ := ctx.Value(routerBudgetKey{}).(bool); routed {
		maxRoutingOutput = min(1024, max(256, limits.OutputTokens/2))
	}
	limit := min(maxRoutingOutput, limits.OutputTokens-16)
	if limit < 16 {
		return result, fmt.Errorf("%w：输出预算需同时容纳分类和回复（至少32个Token）", ErrClassification)
	}
	// Reserve a durable identity before dispatch. An uncertain request after a
	// process/storage failure is explicit and is never automatically replayed.
	if _, err = s.db.ExecContext(ctx, `INSERT INTO submission_classifications(request_key,fingerprint,conversation_id,state,requests,output_used,created_at)
VALUES(?,?,?,'running',1,?,?)`, key, fingerprint, conversationID, limit, now()); err != nil {
		return result, err
	}
	started := time.Now()
	classifyCtx, cancel := context.WithTimeout(ctx, min(8*time.Second, limits.ActiveTime, limits.ModelTimeout))
	decision, classifyErr := classify(classifyCtx, text, history, limit)
	if cause := classifyCtx.Err(); cause != nil {
		if errors.Is(cause, context.DeadlineExceeded) {
			classifyErr = errors.New("委托分类超时，请重新发送")
		} else {
			classifyErr = errors.New("委托分类已取消，请重新发送")
		}
	}
	cancel()
	result.DelegationResult = decision
	if decision.Requests > 0 {
		result.activeMS = time.Since(started).Milliseconds()
		result.outputUsed = limit
		if decision.Usage != nil && decision.Usage.OutputTokens != nil {
			result.outputUsed = int(*decision.Usage.OutputTokens)
		}
	}
	if decision.Requests < 0 || decision.Requests > 1 || result.outputUsed < 0 || result.outputUsed > limit {
		classifyErr = errors.New("分类返回的用量超出已分配预算")
	}
	state, failure = "completed", ""
	if classifyErr != nil {
		state, failure = "failed", classifyErr.Error()
	}
	var inputTokens, outputTokens *int64
	if decision.Usage != nil {
		inputTokens, outputTokens = decision.Usage.InputTokens, decision.Usage.OutputTokens
	}
	if decision.Route != nil && classifyErr == nil {
		encoded, err := json.Marshal(decision.Route)
		if err != nil {
			return result, err
		}
		route = string(encoded)
	}
	if decision.RouterModel != nil {
		encoded, err := json.Marshal(decision.RouterModel)
		if err != nil {
			return result, err
		}
		model = string(encoded)
	}
	// Request cancellation must not discard already incurred usage. Bound the
	// independent receipt write, then leave uncertain identity intact on failure.
	persistCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	_, err = s.db.ExecContext(persistCtx, `UPDATE submission_classifications
SET state=?,is_task=?,requests=?,input_tokens=?,output_tokens=?,active_ms=?,output_used=?,error=?,route=?,router_model_provenance=? WHERE request_key=? AND fingerprint=?`,
		state, decision.Task, decision.Requests, inputTokens, outputTokens, result.activeMS, result.outputUsed, failure, route, model, key, fingerprint)
	if err != nil {
		return result, fmt.Errorf("%w；分类回执暂未保存，不会自动重复请求，可明确选择重新判断并发送", ErrClassificationUnconfirmed)
	}
	if classifyErr != nil {
		return result, fmt.Errorf("%w：%s", ErrClassification, failure)
	}
	return result, nil
}

func attachClassification(tx *sql.Tx, runID, key string, classification submissionClassification) error {
	if classification.Requests > 0 {
		if _, err := tx.Exec(`UPDATE runs SET usage_tracked=1,model_requests=?,admission_requests=?,admission_output_used=?,active_ms=? WHERE id=?`,
			classification.Requests, classification.Requests, classification.outputUsed, classification.activeMS, runID); err != nil {
			return err
		}
		if err := recordModelUsage(tx, runID, classification.Usage); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("UPDATE submission_classifications SET run_id=? WHERE request_key=?", runID, key); err != nil {
		return err
	}
	return addEvent(tx, runID, "submission.classified", map[string]any{
		"task": classification.Task, "model_requests": classification.Requests,
		"usage": classification.Usage, "active_ms": classification.activeMS, "output_budget_used": classification.outputUsed,
	})
}
