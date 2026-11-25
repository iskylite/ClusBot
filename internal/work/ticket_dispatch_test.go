package work

// B 步验收：测试工单系统的受控写路径（E03 首个切片）。
//
// 覆盖验收点：
//   - Operation → 最终核权 → 提交派发记录 → 真实调用提供者 → 回执 → 独立验证；
//   - 用 provider 侧记录核对实际写入次数（不能只听 ClusBot 自己说写成功）；
//   - 重复回调不重复建单（幂等）；
//   - 回执丢失/未知转待核对（不断言未发送）；
//   - 工单关闭未恢复时资产不误恢复（独立效果验证）；
//   - 未授权派发被拒。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeTicketSystem 是一个可控测试工单系统（独立 HTTP 服务，对 ClusBot 是外部系统）。
type fakeTicketSystem struct {
	*httptest.Server
	mu          sync.Mutex
	tickets     map[string]*fakeTicket
	createCalls int32 // provider 侧实际写入次数
	idempotency map[string]string
	nextID      int32
	// 故障注入。
	dropResponse atomic.Bool // 已接收丢响应
	closeState   string
	effectOK     atomic.Bool
}

type fakeTicket struct {
	ID            string
	State         string
	StateVersion  int
	ObjectRef     string
	FaultEvidence string
}

func newFakeTicketSystem() *fakeTicketSystem {
	f := &fakeTicketSystem{tickets: map[string]*fakeTicket{}, idempotency: map[string]string{}, closeState: "closed"}
	f.effectOK.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/tickets", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.createCalls, 1)
		var in struct {
			ObjectRef      string `json:"object_ref"`
			FaultEvidence  string `json:"fault_evidence"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		// provider 侧幂等：同 idempotency_key 返回原工单。
		if existingID, ok := f.idempotency[in.IdempotencyKey]; ok {
			t := *f.tickets[existingID]
			f.mu.Unlock()
			f.writeTicket(w, &t, 200)
			return
		}
		f.nextID++
		id := "EXT-" + strconv.Itoa(int(f.nextID))
		t := &fakeTicket{ID: id, State: "open", StateVersion: 1, ObjectRef: in.ObjectRef, FaultEvidence: in.FaultEvidence}
		f.tickets[id] = t
		f.idempotency[in.IdempotencyKey] = id
		snapshot := *t
		f.mu.Unlock()
		// 故障注入：已接收丢响应。
		if f.dropResponse.Load() {
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				conn.Close()
				return
			}
		}
		f.writeTicket(w, &snapshot, 201)
	})
	mux.HandleFunc("/api/v1/tickets/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/tickets/")
		f.mu.Lock()
		t, ok := f.tickets[id]
		if ok {
			// 推进状态（模拟外部处理）。
			if t.State == "open" {
				t.State = "in_progress"
				t.StateVersion++
			} else if t.State == "in_progress" {
				t.State = f.closeState
				t.StateVersion++
			}
		}
		if !ok {
			f.mu.Unlock()
			w.WriteHeader(404)
			return
		}
		snapshot := *t
		f.mu.Unlock()
		f.writeTicket(w, &snapshot, 200)
	})
	f.Server = httptest.NewServer(mux)
	return f
}

func (f *fakeTicketSystem) writeTicket(w http.ResponseWriter, t *fakeTicket, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"ticket_id": t.ID, "state": t.State, "state_version": t.StateVersion, "effect_verified": f.effectOK.Load(),
	})
}

// createCount 返回 provider 侧实际写入次数（用于核对，不能只听 ClusBot 说）。
func (f *fakeTicketSystem) createCount() int { return int(atomic.LoadInt32(&f.createCalls)) }

func setupB(t *testing.T, name string) (*Store, *fakeTicketSystem, *TicketProvider) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	nowV := now()
	tx, _ := store.db.Begin()
	upsertAsset(tx, OpsAsset{ObjectRef: "node-1", Kind: "node", Identity: "ip1", State: AssetStateMaintenance, Revision: 1, OwnershipEpoch: 1, CurrentOwner: "user:ops"}, nowV)
	// 授权：维修接管（repair）+ 派发（dispatch）。
	grantAuthorization(tx, Authorization{Subject: "user:ops", Action: "repair", ObjectRef: "node-1", ValidFrom: nowV, Owner: "ops"}, nowV)
	grantAuthorization(tx, Authorization{Subject: "user:ops", Action: "dispatch", ObjectRef: "ticket.create", ValidFrom: nowV, Owner: "ops"}, nowV)
	tx.Commit()
	fake := newFakeTicketSystem()
	t.Cleanup(fake.Close)
	provider, err := NewTicketProvider(fake.URL, "ticket-system", "test-cred")
	if err != nil {
		t.Fatal(err)
	}
	return store, fake, provider
}

func TestBTicketControlledWritePath(t *testing.T) {
	s, fake, provider := setupB(t, "b.db")
	ctx := context.Background()

	t.Run("operation_to_real_write_and_verify", func(t *testing.T) {
		// 1. 提交工单创建（Operation → 最终核权）。
		sub, err := s.SubmitTicketCreate(ctx, TicketCreateRequest{
			RequestKey: "b-req-1", ObjectRef: "node-1", FaultEvidence: "网卡故障", Subject: "user:ops",
		})
		if err != nil {
			t.Fatal(err)
		}
		if sub.OperationID == "" {
			t.Fatalf("no operation: %+v", sub)
		}
		// 2. 派发（提交派发记录 → 真实调用提供者）。
		disp, err := s.DispatchTicketCreate(ctx, sub.OperationID, "user:ops", "网卡故障", provider)
		if err != nil {
			t.Fatal(err)
		}
		if disp.ExternalTicketID == "" {
			t.Fatalf("no external ticket: %+v", disp)
		}
		// 用 provider 侧记录核对实际写入次数 = 1（不能只听 ClusBot 说）。
		if fake.createCount() != 1 {
			t.Errorf("provider create calls=%d, want 1（写入次数核对）", fake.createCount())
		}
		// 本地工单记录已建。
		ticket, err := readTicket(s.db, disp.TicketID)
		if err != nil {
			t.Fatal(err)
		}
		if ticket.ExternalTicketID != disp.ExternalTicketID {
			t.Errorf("local ticket external=%s, want %s", ticket.ExternalTicketID, disp.ExternalTicketID)
		}
		// 3. 轮询驱动（回执 → 回写）。置到期让轮询真实驱动。
		s.db.Exec("UPDATE operation_jobs SET next_query_at=0 WHERE operation_id=?", sub.OperationID)
		if n, _ := s.PollTicketJobs(ctx, "user:ops", provider, 10); n != 1 {
			t.Errorf("poll1 n=%d, want 1", n)
		}
		s.db.Exec("UPDATE operation_jobs SET next_query_at=0 WHERE operation_id=?", sub.OperationID)
		if n, _ := s.PollTicketJobs(ctx, "user:ops", provider, 10); n != 1 {
			t.Errorf("poll2 n=%d, want 1", n)
		}
		ticket, _ = readTicket(s.db, disp.TicketID)
		if ticket.State != "closed" {
			t.Errorf("ticket state after polls=%s, want closed（轮询闭环驱动）", ticket.State)
		}
		// Operation 随工单 closed → succeeded（回执成功≠恢复，ticket 仍需独立验证）。
		op, _ := readOperation(s.db, sub.OperationID)
		if op.State != RuntimeOperationSucceeded {
			t.Errorf("operation state=%s, want succeeded", op.State)
		}
		// 4. 独立效果验证（不采信工单自报）。
		checker := func(ctx context.Context, objectRef string) (bool, string, error) {
			return fake.effectOK.Load(), "独立查询设备健康", nil
		}
		if err := s.VerifyTicketClosed(ctx, disp.TicketID, "user:ops", checker); err != nil {
			t.Fatalf("independent verify: %v", err)
		}
		ticket, _ = readTicket(s.db, disp.TicketID)
		if ticket.State != "verified" {
			t.Errorf("ticket state=%s, want verified（独立效果验证通过）", ticket.State)
		}
	})

	t.Run("duplicate_callback_not_duplicate_ticket", func(t *testing.T) {
		// 重复提交同请求键 → 同一 Operation，不重复真实写入。
		sub1, _ := s.SubmitTicketCreate(ctx, TicketCreateRequest{RequestKey: "b-req-2", ObjectRef: "node-1", FaultEvidence: "电源故障", Subject: "user:ops"})
		sub2, _ := s.SubmitTicketCreate(ctx, TicketCreateRequest{RequestKey: "b-req-2", ObjectRef: "node-1", FaultEvidence: "电源故障", Subject: "user:ops"})
		if sub1.OperationID != sub2.OperationID {
			t.Errorf("same key not idempotent: %s vs %s", sub1.OperationID, sub2.OperationID)
		}
		before := fake.createCount()
		s.DispatchTicketCreate(ctx, sub1.OperationID, "user:ops", "电源故障", provider)
		// 重复派发（模拟重投）→ 幂等，不重复真实调用。
		s.DispatchTicketCreate(ctx, sub1.OperationID, "user:ops", "电源故障", provider)
		if got := fake.createCount() - before; got != 1 {
			t.Errorf("duplicate dispatch created %d provider writes, want 1（重复回调不重复建单）", got)
		}
	})

	t.Run("lost_response_goes_to_reconcile_not_retry", func(t *testing.T) {
		// 已接收丢响应：转待核对（不断言未发送，不盲目重发）。
		fake.dropResponse.Store(true)
		sub, _ := s.SubmitTicketCreate(ctx, TicketCreateRequest{RequestKey: "b-req-3", ObjectRef: "node-1", FaultEvidence: "风扇故障", Subject: "user:ops"})
		disp, err := s.DispatchTicketCreate(ctx, sub.OperationID, "user:ops", "风扇故障", provider)
		fake.dropResponse.Store(false)
		if err != nil {
			t.Fatal(err)
		}
		if disp.State != string(RuntimeOperationReconcile) {
			t.Errorf("lost response state=%s, want reconcile（回执丢失转待核对）", disp.State)
		}
		op, _ := readOperation(s.db, sub.OperationID)
		if op.State != RuntimeOperationReconcile {
			t.Errorf("operation state=%s, want reconcile", op.State)
		}
	})

	t.Run("closed_but_not_recovered_not_restored", func(t *testing.T) {
		// 工单关闭但故障仍在：独立效果验证失败，资产不误恢复。
		fake.effectOK.Store(false)
		sub, _ := s.SubmitTicketCreate(ctx, TicketCreateRequest{RequestKey: "b-req-4", ObjectRef: "node-1", FaultEvidence: "内存故障", Subject: "user:ops"})
		disp, _ := s.DispatchTicketCreate(ctx, sub.OperationID, "user:ops", "内存故障", provider)
		s.PollTicketJobs(ctx, "user:ops", provider, 10)
		s.PollTicketJobs(ctx, "user:ops", provider, 10)
		checker := func(ctx context.Context, objectRef string) (bool, string, error) {
			return false, "独立查询：故障仍在", nil
		}
		err := s.VerifyTicketClosed(ctx, disp.TicketID, "user:ops", checker)
		if err == nil {
			t.Error("not-recovered ticket verified (关闭工单但故障仍在时资产不应被误恢复)")
		}
		ticket, _ := readTicket(s.db, disp.TicketID)
		if ticket.State == "verified" {
			t.Error("unrecovered ticket marked verified")
		}
		fake.effectOK.Store(true)
	})

	t.Run("unauthorized_dispatch_denied", func(t *testing.T) {
		// 未授权主体派发被拒。
		_, err := s.SubmitTicketCreate(ctx, TicketCreateRequest{RequestKey: "b-req-5", ObjectRef: "node-1", FaultEvidence: "x", Subject: "user:other"})
		if err == nil {
			t.Error("unauthorized ticket create not denied")
		}
	})
}

// TestBTicketRestartRecovery 重启后从持久 Job 继续轮询（B 步：重启能继续）。
func TestBTicketRestartRecovery(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "b-restart.db")
	fake := newFakeTicketSystem()
	defer fake.Close()
	provider, _ := NewTicketProvider(fake.URL, "ticket-system", "test-cred")
	var operationID, ticketID string
	nowV := now()
	// 第一段：提交并派发（不轮询，模拟派发后中断）。
	func() {
		s, err := Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		tx, _ := s.db.Begin()
		upsertAsset(tx, OpsAsset{ObjectRef: "node-1", Kind: "node", Identity: "ip1", State: AssetStateMaintenance, Revision: 1, OwnershipEpoch: 1}, nowV)
		grantAuthorization(tx, Authorization{Subject: "user:ops", Action: "repair", ObjectRef: "node-1", ValidFrom: nowV, Owner: "ops"}, nowV)
		grantAuthorization(tx, Authorization{Subject: "user:ops", Action: "dispatch", ObjectRef: "ticket.create", ValidFrom: nowV, Owner: "ops"}, nowV)
		tx.Commit()
		sub, _ := s.SubmitTicketCreate(context.Background(), TicketCreateRequest{RequestKey: "b-restart", ObjectRef: "node-1", FaultEvidence: "网卡", Subject: "user:ops"})
		disp, _ := s.DispatchTicketCreate(context.Background(), sub.OperationID, "user:ops", "网卡", provider)
		operationID, ticketID = sub.OperationID, disp.TicketID
		// 中断：不轮询，Close。
	}()
	// 重启：RecoverRuntime 恢复，从持久 Job 继续轮询（重启按持久 Job ID 继续）。
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.RecoverRuntime(context.Background(), nowV); err != nil {
		t.Fatal(err)
	}
	// Operation 保持 running（未被误判为未执行）。
	op, _ := readOperation(s2.db, operationID)
	if op.State != RuntimeOperationRunning {
		t.Errorf("restart operation state=%s, want running（重启不误判未执行）", op.State)
	}
	// 从持久 Job 继续轮询到 closed。
	s2.db.Exec("UPDATE operation_jobs SET next_query_at=0 WHERE operation_id=?", operationID)
	if n, _ := s2.PollTicketJobs(context.Background(), "user:ops", provider, 10); n != 1 {
		t.Fatalf("restart poll1 n=%d, want 1", n)
	}
	s2.db.Exec("UPDATE operation_jobs SET next_query_at=0 WHERE operation_id=?", operationID)
	s2.PollTicketJobs(context.Background(), "user:ops", provider, 10)
	ticket, _ := readTicket(s2.db, ticketID)
	if ticket.State != "closed" {
		t.Errorf("restart ticket state=%s, want closed（重启从持久Job继续）", ticket.State)
	}
	op, _ = readOperation(s2.db, operationID)
	if op.State != RuntimeOperationSucceeded {
		t.Errorf("restart operation=%s, want succeeded", op.State)
	}
	// provider 侧只写入一次（重启不重复建单）。
	if fake.createCount() != 1 {
		t.Errorf("provider create calls=%d after restart, want 1（重启不重复写入）", fake.createCount())
	}
}
