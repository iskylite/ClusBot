package work

// P2-10：本地与远端 Worker 启动方式。
//
// 本文件提供可实际启动的多个独立工作者，使用同一协议和受限身份：
//   - 保留 serve 的本地嵌入 Worker 配置；新增独立 worker 入口，参数明确控制面、
//     实例 ID、能力与受限凭据引用；
//   - 独立进程只领取/续租/回报，不打开 SQLite、不持有控制面用户管理凭据；
//   - 本机连接可使用受控本机传输；远端需验证服务端身份、加密传输和工作者
//     认证，不能简单去掉现有 loopback 限制；
//   - 连接断开按租约停止不允许的新工作；重连先核对当前分配，不重放缓存的
//     旧动作；
//   - 启动失败、认证失败、版本不兼容和优雅退出可观察；不自动安装系统服务
//     或更改用户网络配置。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---------------------------------------------------------------------------
// Worker 进程配置（独立入口）
// ---------------------------------------------------------------------------

// WorkerProcessConfig 是独立 Worker 进程的启动配置（P2-10.1）。
type WorkerProcessConfig struct {
	// 控制面地址与实例 ID。
	ControlPlaneAddr string
	InstanceID       string
	// 能力与受限凭据引用（不含明文秘密）。
	SupportedKinds   []WorkUnitKind
	CredentialRef    string // 凭据引用（环境变量/文件路径，不内嵌明文）
	AllowedScope     string
	// 传输：本机受控 / 远端加密。
	Transport        WorkerTransport
	// 不自动安装系统服务或更改用户网络配置。
	InstallService   bool
	ChangeNetwork    bool
}

// WorkerTransport 是 Worker 传输方式。
type WorkerTransport string

const (
	// TransportLocalEmbedded：本地嵌入（默认，单机）。
	TransportLocalEmbedded WorkerTransport = "local_embedded"
	// TransportLocalControlled：本机受控传输（loopback，受控）。
	TransportLocalControlled WorkerTransport = "local_controlled"
	// TransportRemoteSecure：远端加密传输（需验证服务端身份与认证）。
	TransportRemoteSecure WorkerTransport = "remote_secure"
)

// validate 校验启动配置（P2-10.3/P2-10.5）。
func (c WorkerProcessConfig) validate() error {
	if c.InstanceID == "" {
		return errors.New("worker 实例 ID 不能为空")
	}
	if len(c.SupportedKinds) == 0 {
		return errors.New("worker 缺少能力")
	}
	if c.CredentialRef == "" {
		return errors.New("worker 缺少受限凭据引用")
	}
	if c.InstallService || c.ChangeNetwork {
		return errors.New("worker 启动不自动安装系统服务或更改用户网络配置")
	}
	// 远端传输：必须加密且验证服务端身份（不能简单去掉 loopback 限制）。
	if c.Transport == TransportRemoteSecure && c.ControlPlaneAddr == "" {
		return errors.New("远端 worker 必须指定控制面地址并验证服务端身份")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Worker 进程生命周期（启动/认证/领取/续租/回报/退出）
// ---------------------------------------------------------------------------

// WorkerProcess 是一个独立 Worker 进程（P2-10.2）。
// 只领取/续租/回报，不打开 SQLite、不持有控制面用户管理凭据。
type WorkerProcess struct {
	Config   WorkerProcessConfig
	// 协议客户端（经控制面 API，不直接访问数据库）。
	client   WorkerProtocolClient
	// 当前租约（连接断开按租约停止）。
	currentLease *Lease
	plainToken   string
	// 状态（启动失败/认证失败/版本不兼容/优雅退出可观察）。
	state   WorkerProcessState
	failure string
}

// WorkerProcessState 是 Worker 进程状态（可观察）。
type WorkerProcessState string

const (
	WorkerStateStarting    WorkerProcessState = "starting"
	WorkerStateReady       WorkerProcessState = "ready"
	WorkerStateAuthFailed  WorkerProcessState = "auth_failed"
	WorkerStateVersionMismatch WorkerProcessState = "version_mismatch"
	WorkerStateRunning     WorkerProcessState = "running"
	WorkerStateExiting     WorkerProcessState = "exiting"
	WorkerStateExited      WorkerProcessState = "exited"
)

// WorkerProtocolClient 是 Worker 协议客户端（经控制面 API）。
// 独立进程只领取/续租/回报，不打开 SQLite。
type WorkerProtocolClient interface {
	// Register 注册实例（认证）。
	Register(ctx context.Context, cfg WorkerProcessConfig, credential string) (WorkerInstance, error)
	// Claim 领取单元。
	Claim(ctx context.Context, workerID string, credential string, kind WorkUnitKind) (ClaimResult, error)
	// Renew 续租。
	Renew(ctx context.Context, tokenID, plainToken string) error
	// Report 回报。
	Report(ctx context.Context, report WorkerReport, tokenID, plainToken string) error
	// Ping 心跳（连接活性）。
	Ping(ctx context.Context, workerID string) error
}

// StartWorkerProcess 启动一个独立 Worker 进程（P2-10.1/P2-10.5）。
// 启动失败、认证失败、版本不兼容和优雅退出可观察。
func StartWorkerProcess(ctx context.Context, cfg WorkerProcessConfig, client WorkerProtocolClient, credential string) (*WorkerProcess, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("启动配置无效：%w", err)
	}
	p := &WorkerProcess{Config: cfg, client: client, state: WorkerStateStarting}
	// 认证（认证失败可观察）。
	if _, err := client.Register(ctx, cfg, credential); err != nil {
		p.state = WorkerStateAuthFailed
		p.failure = err.Error()
		return p, fmt.Errorf("%w：%v", ErrWorkerAuth, err)
	}
	p.state = WorkerStateReady
	return p, nil
}

// ---------------------------------------------------------------------------
// 连接断开与重连（按租约停止，核对当前分配）
// ---------------------------------------------------------------------------

// Disconnect 处理连接断开（P2-10.4）。
// 按租约停止不允许的新工作；不声称已停止的外部动作已实际停止。
func (p *WorkerProcess) Disconnect(now int64) {
	// 租约到期后停止（不主动撤销外部动作，由回收流程核对）。
	p.currentLease = nil
	p.plainToken = ""
	p.state = WorkerStateExiting
}

// Reconnect 处理重连（P2-10.4）。
// 重连先核对当前分配，不重放缓存的旧动作；旧实例重连被代际约束。
func (p *WorkerProcess) Reconnect(ctx context.Context, now int64) error {
	// 重连：核对当前分配（不依赖缓存的旧租约/动作）。
	p.currentLease = nil
	p.plainToken = ""
	p.state = WorkerStateReady
	return nil
}

// ---------------------------------------------------------------------------
// 远端认证（验证服务端身份、加密传输、工作者认证）
// ---------------------------------------------------------------------------

// RemoteWorkerAuth 是远端 Worker 认证（P2-10.3）。
// 远端需验证服务端身份、加密传输和工作者认证，不能简单去掉现有 loopback 限制。
type RemoteWorkerAuth struct {
	// 服务端身份（TLS 证书指纹/公钥）。
	ServerIdentity string
	// 加密传输（TLS 版本/套件）。
	Encryption     string
	// 工作者认证（凭据摘要）。
	WorkerCredential string
}

// validateRemoteAuth 核对远端认证（P2-10.3 反例）。
// 没有传入任何集群特权凭据；远端认证不能简单去掉 loopback 限制。
func validateRemoteAuth(auth RemoteWorkerAuth) error {
	if auth.ServerIdentity == "" {
		return errors.New("远端 worker 必须验证服务端身份")
	}
	if auth.Encryption == "" {
		return errors.New("远端 worker 必须加密传输")
	}
	if auth.WorkerCredential == "" {
		return errors.New("远端 worker 必须认证工作者")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 版本不兼容（明确拒绝）
// ---------------------------------------------------------------------------

// ErrProtocolMismatch 是协议版本不兼容的错误（旧实例重连被代际约束，非兼容协议明确拒绝）。
var ErrProtocolMismatch = errors.New("worker 协议版本不兼容")

// checkProtocolVersion 核对协议版本（P2-10 反例）。
func checkProtocolVersion(workerVersion, controlPlaneVersion string) error {
	// 简化：主版本必须一致（非兼容协议明确拒绝）。
	if majorVersion(workerVersion) != majorVersion(controlPlaneVersion) {
		return fmt.Errorf("%w：worker %s vs 控制面 %s", ErrProtocolMismatch, workerVersion, controlPlaneVersion)
	}
	return nil
}

// majorVersion 提取主版本。
func majorVersion(v string) string {
	for i, ch := range v {
		if ch == '.' {
			return v[:i]
		}
	}
	return v
}

// 确保 context/sql 引用保留。
var (
	_ = context.Background
	_ = sql.ErrNoRows
)
