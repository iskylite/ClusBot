package httpapi

import (
	"net/http"
	"time"
)

type approvalView struct {
	ApprovalID       string `json:"approval_id"`
	Subject          string `json:"subject"`
	Action           string `json:"action"`
	ObjectRef        string `json:"object_ref"`
	ParamsDigest     string `json:"params_digest"`
	ContractRevision int    `json:"contract_revision"`
	PolicyVersion    int    `json:"policy_version"`
	AssetRevision    int    `json:"asset_revision"`
	DecidedBy        string `json:"decided_by"`
	ExpiresAt        int64  `json:"expires_at"`
	Decision         string `json:"decision"`
	CreatedAt        int64  `json:"created_at"`
}

type publicationView struct {
	ID           string `json:"id"`
	TaskID       string `json:"task_id"`
	ArtifactID   string `json:"artifact_id"`
	RunID        string `json:"run_id"`
	Filename     string `json:"filename"`
	Digest       string `json:"sha256"`
	Status       string `json:"status"`
	Decision     string `json:"decision"`
	Detail       string `json:"detail,omitempty"`
	ExpiresAt    int64  `json:"expires_at"`
	TaskRevision int    `json:"task_revision"`
}

type decisionsResponse struct {
	Approvals    []approvalView    `json:"approvals"`
	Publications []publicationView `json:"publications"`
	SnapshotAt   int64             `json:"snapshot_at"`
}

// listDecisions 汇总“需要人拍板”的真实记录：领域确认 + 文件写入审批。
// 不接受客户端指定 subject，统一用当前会话主体，避免越权读取。
func (s Server) listDecisions(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UnixMilli()
	approvals, err := s.Store.ListPendingApprovals(r.Context(), s.principal(r), now)
	if err != nil {
		writeError(w, err)
		return
	}
	publications, err := s.Store.PendingPublications(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	out := decisionsResponse{
		Approvals:    make([]approvalView, 0, len(approvals)),
		Publications: make([]publicationView, 0, len(publications)),
		SnapshotAt:   now,
	}
	for _, a := range approvals {
		out.Approvals = append(out.Approvals, approvalView{
			ApprovalID: a.ApprovalID, Subject: a.Subject, Action: a.Action,
			ObjectRef: a.ObjectRef, ParamsDigest: a.ParamsDigest,
			ContractRevision: a.ContractRevision, PolicyVersion: a.PolicyVersion,
			AssetRevision: a.AssetRevision, DecidedBy: a.DecidedBy,
			ExpiresAt: a.ExpiresAt, Decision: a.Decision, CreatedAt: a.CreatedAt,
		})
	}
	for _, p := range publications {
		out.Publications = append(out.Publications, publicationView{
			ID: p.ID, TaskID: p.TaskID, ArtifactID: p.ArtifactID, RunID: p.RunID,
			Filename: p.Filename, Digest: p.Digest, Status: p.Status,
			Decision: p.Decision, Detail: p.Detail, ExpiresAt: p.ExpiresAt,
			TaskRevision: p.TaskRevision,
		})
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s Server) decideApproval(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Decision string `json:"decision"`
	}
	if !decode(w, r, &in) {
		return
	}
	a, err := s.Store.DecideApproval(r.Context(), r.PathValue("id"), in.Decision, s.principal(r))
	if err != nil {
		writeError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, approvalView{
		ApprovalID: a.ApprovalID, Subject: a.Subject, Action: a.Action,
		ObjectRef: a.ObjectRef, ParamsDigest: a.ParamsDigest,
		ContractRevision: a.ContractRevision, PolicyVersion: a.PolicyVersion,
		AssetRevision: a.AssetRevision, DecidedBy: a.DecidedBy,
		ExpiresAt: a.ExpiresAt, Decision: a.Decision, CreatedAt: a.CreatedAt,
	})
}
