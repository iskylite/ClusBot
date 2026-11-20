package model

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"clusbot/internal/work"
)

type TaskWorkerRoleInput struct {
	Action                   string `json:"action"`
	Revision                 int    `json:"revision"`
	ProfileID                string `json:"profile_id,omitempty"`
	ProfileRevision          int    `json:"profile_revision,omitempty"`
	ConfirmTaskContextEgress bool   `json:"confirm_task_context_egress,omitempty"`
}

func (s *Settings) ReadTaskWorkerRole(ctx context.Context) (BenchmarkAdmissionRoleView, error) {
	v, _, _, err := s.modelRole(ctx, work.TaskWorkerModelRole)
	return v, err
}

func (s *Settings) SetTaskWorkerRole(ctx context.Context, in TaskWorkerRoleInput) (BenchmarkAdmissionRoleView, error) {
	if in.Action == "bind_profile" && !in.ConfirmTaskContextEgress {
		return BenchmarkAdmissionRoleView{}, InvalidSettings("请明确同意所选模型处理事项正文及历史、要求、已选材料及可用图片、匹配的记忆、流程成果、技能和经验，以及已授权读取工具和本轮模型/工具回合")
	}
	return s.setModelRole(ctx, work.TaskWorkerModelRole, BenchmarkAdmissionRoleInput{
		Action: in.Action, Revision: in.Revision, ProfileID: in.ProfileID, ProfileRevision: in.ProfileRevision,
		ConfirmRequestTextEgress: in.ConfirmTaskContextEgress,
	})
}

func (s *Settings) ResolveTaskRunProvider(ctx context.Context) (work.TaskRunProvider, error) {
	if s.Demo {
		return nil, nil // Preserve the existing demo/task handling, including authorized direct reads.
	}
	v, value, key, err := s.modelRole(ctx, work.TaskWorkerModelRole)
	if err != nil {
		return nil, err
	}
	if v.Source == "legacy_default" {
		p, err := s.provider(ctx)
		if err != nil {
			return nil, err
		}
		return pinnedTaskRunProvider{p}, nil
	}
	p, err := s.profileForModelRole(v, value, key, work.ErrTaskWorkerModelRoleUnavailable)
	if err != nil {
		return nil, err
	}
	caps, err := json.Marshal(struct {
		Selected  work.ModelAttributes
		Inherited *roleRequestEnvelope
	}{p.attributes, p.roleEnvelope})
	if err != nil {
		return nil, err
	}
	p.checkpointScope = fmt.Sprintf("task_worker/v1:%d:%s:%d:%x", v.Revision, v.ProfileID, v.ProfileRevision, sha256.Sum256(caps))
	return pinnedTaskRunProvider{p}, nil
}

// Both resume checkpoints and all model entry points share this immutable
// provider. The wrapper does not add/remove the underlying adapter's tools.
type pinnedTaskRunProvider struct{ *Provider }

func (p pinnedTaskRunProvider) TextRunProvider(context.Context, bool) (work.Provider, string, error) {
	binding, _ := p.ReadCheckpointIdentity()
	return p.Provider, binding + "\ntask/v1", nil
}

func (p pinnedTaskRunProvider) TaskReadModel(context.Context) (work.TaskReadModel, error) {
	return p.Provider, nil
}

func (p pinnedTaskRunProvider) MultiTaskReadModel(context.Context) (work.MultiTaskReadModel, error) {
	return p.Provider, nil
}
