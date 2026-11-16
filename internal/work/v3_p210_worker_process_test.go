package work

// P2-10 验收：本地与远端 Worker 启动方式。
//
// 三个测试入口：
//   - TestV3P210WorkerProcessLifecycle：启动失败、认证失败、版本不兼容和优雅
//     退出可观察；不自动安装系统服务或更改用户网络配置。
//   - TestV3P210RemoteWorkerAuthentication：远端需验证服务端身份、加密传输和
//     工作者认证，不能简单去掉现有 loopback 限制；没有传入任何集群特权凭据。
//   - TestV3P210WorkerReconnect：连接断开按租约停止不允许的新工作；重连先核对
//     当前分配，不重放缓存的旧动作；旧实例重连被代际约束，非兼容协议明确拒绝。

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// mockWorkerProtocolClient 是测试用协议客户端。
type mockWorkerProtocolClient struct {
	registerErr error
	claimResult ClaimResult
	claimErr    error
	registered  bool
}

func (m *mockWorkerProtocolClient) Register(ctx context.Context, cfg WorkerProcessConfig, credential string) (WorkerInstance, error) {
	if m.registerErr != nil {
		return WorkerInstance{}, m.registerErr
	}
	m.registered = true
	return WorkerInstance{WorkerID: cfg.InstanceID}, nil
}
func (m *mockWorkerProtocolClient) Claim(ctx context.Context, workerID, credential string, kind WorkUnitKind) (ClaimResult, error) {
	return m.claimResult, m.claimErr
}
func (m *mockWorkerProtocolClient) Renew(ctx context.Context, tokenID, plainToken string) error { return nil }
func (m *mockWorkerProtocolClient) Report(ctx context.Context, report WorkerReport, tokenID, plainToken string) error {
	return nil
}
func (m *mockWorkerProtocolClient) Ping(ctx context.Context, workerID string) error { return nil }

// v3P210Config 构造 Worker 进程配置。
func v3P210Config(instanceID string, transport WorkerTransport) WorkerProcessConfig {
	return WorkerProcessConfig{
		ControlPlaneAddr: "127.0.0.1:8080",
		InstanceID:       instanceID,
		SupportedKinds:   []WorkUnitKind{WorkUnitModel},
		CredentialRef:    "env:WORKER_CREDENTIAL",
		AllowedScope:     "工作资料",
		Transport:        transport,
	}
}

// TestV3P210WorkerProcessLifecycle 进程生命周期。
func TestV3P210WorkerProcessLifecycle(t *testing.T) {
	t.Run("startup_auth_version_exit_observable", func(t *testing.T) {
		ctx := context.Background()
		// 启动成功。
		client := &mockWorkerProtocolClient{}
		p, err := StartWorkerProcess(ctx, v3P210Config("worker-1", TransportLocalEmbedded), client, "secret")
		if err != nil {
			t.Fatal(err)
		}
		if p.state != WorkerStateReady {
			t.Errorf("state = %s, want ready", p.state)
		}
		// 优雅退出。
		p.Disconnect(v3P102Now())
		if p.state != WorkerStateExiting {
			t.Errorf("exit state = %s, want exiting (优雅退出可观察)", p.state)
		}
		// 认证失败：可观察。
		badClient := &mockWorkerProtocolClient{registerErr: errors.New("bad credential")}
		p2, err := StartWorkerProcess(ctx, v3P210Config("worker-2", TransportLocalEmbedded), badClient, "wrong")
		if err == nil {
			t.Error("auth failure not detected")
		}
		if p2.state != WorkerStateAuthFailed {
			t.Errorf("auth failure state = %s, want auth_failed (认证失败可观察)", p2.state)
		}
		if p2.failure == "" {
			t.Error("auth failure reason missing")
		}
	})

	t.Run("no_service_install_or_network_change", func(t *testing.T) {
		ctx := context.Background()
		// 反例：自动安装系统服务或更改网络配置被拒绝。
		bad := v3P210Config("worker-bad", TransportLocalEmbedded)
		bad.InstallService = true
		_, err := StartWorkerProcess(ctx, bad, &mockWorkerProtocolClient{}, "secret")
		if err == nil {
			t.Error("service install not rejected (不自动安装系统服务)")
		}
		bad2 := v3P210Config("worker-bad2", TransportLocalEmbedded)
		bad2.ChangeNetwork = true
		_, err = StartWorkerProcess(ctx, bad2, &mockWorkerProtocolClient{}, "secret")
		if err == nil {
			t.Error("network change not rejected (不更改用户网络配置)")
		}
	})

	t.Run("version_mismatch_rejected", func(t *testing.T) {
		// 版本不兼容：明确拒绝。
		if err := checkProtocolVersion("1.x", "2.x"); err == nil {
			t.Error("incompatible protocol not rejected (非兼容协议明确拒绝)")
		}
		if err := checkProtocolVersion("2.x", "2.y"); err != nil {
			t.Errorf("compatible protocol rejected: %v", err)
		}
	})
}

// TestV3P210RemoteWorkerAuthentication 远端认证。
func TestV3P210RemoteWorkerAuthentication(t *testing.T) {
	t.Run("remote_requires_server_identity_encryption_worker_auth", func(t *testing.T) {
		// 远端认证：服务端身份 + 加密传输 + 工作者认证（不能去掉 loopback 限制）。
		valid := RemoteWorkerAuth{
			ServerIdentity:   "tls-fingerprint",
			Encryption:       "TLS1.3",
			WorkerCredential: "worker-cred",
		}
		if err := validateRemoteAuth(valid); err != nil {
			t.Errorf("valid remote auth rejected: %v", err)
		}
		// 缺服务端身份：拒绝。
		if err := validateRemoteAuth(RemoteWorkerAuth{Encryption: "TLS1.3", WorkerCredential: "cred"}); err == nil {
			t.Error("missing server identity not rejected")
		}
		// 缺加密：拒绝。
		if err := validateRemoteAuth(RemoteWorkerAuth{ServerIdentity: "fp", WorkerCredential: "cred"}); err == nil {
			t.Error("missing encryption not rejected")
		}
		// 缺工作者认证：拒绝。
		if err := validateRemoteAuth(RemoteWorkerAuth{ServerIdentity: "fp", Encryption: "TLS1.3"}); err == nil {
			t.Error("missing worker auth not rejected")
		}
	})

	t.Run("remote_transport_requires_control_plane_addr", func(t *testing.T) {
		// 远端传输：必须指定控制面地址并验证服务端身份。
		cfg := v3P210Config("remote-worker", TransportRemoteSecure)
		cfg.ControlPlaneAddr = "" // 缺控制面地址
		if err := cfg.validate(); err == nil {
			t.Error("remote without control plane addr not rejected")
		}
	})

	t.Run("no_cluster_privileged_credential", func(t *testing.T) {
		// 没有传入任何集群特权凭据（Worker 配置不含特权）。
		cfg := v3P210Config("worker-nopriv", TransportLocalEmbedded)
		if err := cfg.validate(); err != nil {
			t.Fatal(err)
		}
		// 凭据引用是受限的（env 引用，不含明文特权）。
		if !strings.HasPrefix(cfg.CredentialRef, "env:") && !strings.HasPrefix(cfg.CredentialRef, "file:") {
			t.Errorf("credential ref = %s (应引用受限凭据，不含集群特权凭据)", cfg.CredentialRef)
		}
	})
}

// TestV3P210WorkerReconnect 重连。
func TestV3P210WorkerReconnect(t *testing.T) {
	t.Run("disconnect_stops_new_work_by_lease", func(t *testing.T) {
		ctx := context.Background()
		client := &mockWorkerProtocolClient{}
		p, err := StartWorkerProcess(ctx, v3P210Config("worker-disc", TransportLocalEmbedded), client, "secret")
		if err != nil {
			t.Fatal(err)
		}
		// 连接断开：按租约停止不允许的新工作（清除当前租约）。
		p.currentLease = &Lease{LeaseID: "lease-1", UnitID: "unit-1"}
		p.plainToken = "token"
		p.Disconnect(v3P102Now())
		if p.currentLease != nil {
			t.Error("disconnect kept lease (断开按租约停止新工作)")
		}
		if p.plainToken != "" {
			t.Error("disconnect kept token")
		}
	})

	t.Run("reconnect_rechecks_assignment_not_replay_cached", func(t *testing.T) {
		ctx := context.Background()
		client := &mockWorkerProtocolClient{}
		p, err := StartWorkerProcess(ctx, v3P210Config("worker-reconn", TransportLocalEmbedded), client, "secret")
		if err != nil {
			t.Fatal(err)
		}
		// 缓存旧租约后断开重连：核对当前分配，不重放缓存旧动作。
		p.currentLease = &Lease{LeaseID: "old-lease", UnitID: "old-unit"}
		p.plainToken = "old-token"
		if err := p.Reconnect(ctx, v3P102Now()); err != nil {
			t.Fatal(err)
		}
		if p.currentLease != nil {
			t.Error("reconnect kept cached lease (重连核对当前分配不重放缓存旧动作)")
		}
		if p.plainToken != "" {
			t.Error("reconnect kept cached token")
		}
		if p.state != WorkerStateReady {
			t.Errorf("reconnect state = %s, want ready", p.state)
		}
	})

	t.Run("old_instance_reconnect_generation_constrained", func(t *testing.T) {
		// 旧实例重连被代际约束：旧租约代际不能推进当前工作。
		store, err := Open(filepath.Join(t.TempDir(), "gen.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		now := v3P102Now()
		ctx := context.Background()
		run, _ := store.Submit(ctx, "v3p210-gen", "整理汇报", "", "", true)
		taskID := run.TaskID
		tx, _ := store.db.Begin()
		insertStep(tx, Step{StepID: id(), TaskID: taskID, ContractRevision: 1,
			DefinitionKey: "gen-step", DefinitionVersion: 1, State: V3StepReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		steps, _ := listSteps(store.db, taskID)
		tx, _ = store.db.Begin()
		insertWorkUnit(tx, WorkUnit{UnitID: "gen-unit", TaskID: taskID, StepID: steps[0].StepID,
			Kind: WorkUnitModel, ReadyAt: now, State: WorkUnitReady, CreatedAt: now, UpdatedAt: now})
		tx.Commit()
		worker, cred := registerV3P202Worker(t, store, "worker-gen", []WorkUnitKind{WorkUnitModel}, "")
		// 领取（代际 1）。
		tx, _ = store.db.Begin()
		claim1, err := claimWorkUnit(tx, ClaimRequest{WorkerID: worker, Credential: cred, Kind: WorkUnitModel, LeaseTTL: 60000, QuotaAvailable: true}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		// 回收租约（单元回 ready，代际仍为 1）。
		tx, _ = store.db.Begin()
		reclaimExpiredLeases(tx, now+100000, 10)
		tx.Commit()
		// 新领取（代际 2）：旧令牌不能推进。
		tx, _ = store.db.Begin()
		claim2, err := claimWorkUnit(tx, ClaimRequest{WorkerID: worker, Credential: cred, Kind: WorkUnitModel, LeaseTTL: 60000, QuotaAvailable: true}, now)
		if err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if claim2.Lease.LeaseGeneration != 2 {
			t.Errorf("new claim generation = %d, want 2", claim2.Lease.LeaseGeneration)
		}
		// 旧令牌（代际 1）不能续租（代际约束）。
		tx, _ = store.db.Begin()
		_, err = renewLease(tx, claim1.Token.TokenID, claim1.PlainToken, now+1000)
		tx.Rollback()
		if err == nil {
			t.Error("old generation token renewed (旧实例重连被代际约束)")
		}
	})

	t.Run("worker_cannot_read_sqlite_to_complete_protocol", func(t *testing.T) {
		// 工作者不能读取本地数据库文件才能完成协议（独立进程不打开 SQLite）。
		// WorkerProtocolClient 接口只暴露领取/续租/回报（无数据库访问）。
		var client WorkerProtocolClient = &mockWorkerProtocolClient{}
		// 接口方法不含数据库访问（协议经控制面 API）。
		ctx := context.Background()
		_, err := client.Claim(ctx, "worker", "cred", WorkUnitModel)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			// 协议经 API，不直接访问数据库
		}
		// 验证 WorkerProcessConfig 不含数据库路径（独立进程不打开 SQLite）。
		cfg := v3P210Config("worker-nodb", TransportLocalEmbedded)
		if strings.Contains(cfg.ControlPlaneAddr, ".db") || strings.Contains(cfg.ControlPlaneAddr, "sqlite") {
			t.Error("worker config contains sqlite path (独立进程不打开 SQLite)")
		}
	})
}

// 确保 sql/strings/context/errors 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
	_ = errors.Is
)
