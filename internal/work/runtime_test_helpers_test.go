package work

// P0-04 建立可控时钟、故障屏障与外部账本（测试基础设施）。
//
// 本文件提供后续 F01—F12 故障注入验收所需的可复用设施：
//   - Clock：可控时钟接口，生产默认使用系统时钟，测试可暂停/推进；
//   - Barrier：可暂停的提交/派发边界，用确定性信号而非偶然 sleep 命中竞态；
//   - ProviderLedger：受控提供者的独立账本，记录收到请求、实际生效、作业
//     状态、取消与查询，与 ClusBot 数据库分开保存，支持重复/乱序/同 ID
//     不同内容、响应丢失、工作者终止与重连的注入。
//
// 纪律：生产路径默认禁用测试钩子，不新增公开故障接口；模型输出不能作为
// 测试真值（账本与数据库快照才是）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Clock：可控时钟
// ---------------------------------------------------------------------------

// Clock 是运行时的时间来源。生产默认 SystemClock；测试用 FakeClock 暂停
// 并确定性地推进时间，避免依赖墙钟碰巧命中。
type Clock interface {
	Now() time.Time
}

// SystemClock 是生产默认时钟。
type SystemClock struct{}

// Now 返回当前系统时间。
func (SystemClock) Now() time.Time { return time.Now() }

// FakeClock 是可暂停、可推进的测试时钟。
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock 以指定起点创建测试时钟。
func NewFakeClock(start time.Time) *FakeClock { return &FakeClock{now: start} }

// Now 返回当前（可能被冻结的）时间。
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance 将时钟推进 d，并返回新的当前时间。
func (c *FakeClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// Set 直接设定当前时间。
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// ---------------------------------------------------------------------------
// Barrier：可暂停的提交/派发边界
// ---------------------------------------------------------------------------

// BarrierPoint 标识一个可注入的边界。P0-04.2 定义的点覆盖 F01—F09 的关键
// 提交/派发竞态；具体在途效果在各工作项落地时接入。
type BarrierPoint string

const (
	// BarrierBeforeInputCommit：输入确认提交前。
	BarrierBeforeInputCommit BarrierPoint = "before_input_commit"
	// BarrierAfterInputCommit：输入确认提交后。
	BarrierAfterInputCommit BarrierPoint = "after_input_commit"
	// BarrierAfterDispatchCommit：已派发提交后、外部调用前（F02a）。
	BarrierAfterDispatchCommit BarrierPoint = "after_dispatch_commit"
	// BarrierBeforeExternalCall：外部调用前。
	BarrierBeforeExternalCall BarrierPoint = "before_external_call"
	// BarrierAfterExternalEffect：外部已生效、回执入库前（F02b）。
	BarrierAfterExternalEffect BarrierPoint = "after_external_effect"
	// BarrierBeforeInboxApply：Inbox 应用前。
	BarrierBeforeInboxApply BarrierPoint = "before_inbox_apply"
	// BarrierBeforeWaitClose：Wait 关闭前（F09）。
	BarrierBeforeWaitClose BarrierPoint = "before_wait_close"
	// BarrierLeaseExpired：租约过期点（F04）。
	BarrierLeaseExpired BarrierPoint = "lease_expired"
	// BarrierAfterRevokeCommit：撤权提交后、最终核权前（F07）。
	BarrierAfterRevokeCommit BarrierPoint = "after_revoke_commit"
)

// Barrier 让测试在指定边界暂停并释放，从而以确定顺序复现两种相反的
// 提交顺序，而不是靠 sleep 碰巧命中。每个点是一次性闸门：到达的goroutine
// 阻塞直到 Release 或超时。
type Barrier struct {
	mu     sync.Mutex
	gates  map[BarrierPoint]chan struct{}
	armLog []BarrierPoint
}

// NewBarrier 创建屏障。
func NewBarrier() *Barrier { return &Barrier{gates: map[BarrierPoint]chan struct{}{}} }

// Arm 预置一个边界点：下次经过该点的 goroutine 将阻塞。
// 返回的 release 用于手动放行（等价于调用 Release）。
func (b *Barrier) Arm(point BarrierPoint) (release func()) {
	b.mu.Lock()
	ch := make(chan struct{})
	b.gates[point] = ch
	b.armLog = append(b.armLog, point)
	b.mu.Unlock()
	return func() { b.Release(point) }
}

// Wait 在边界点等待放行；若该点未 Arm 或已放行则立即返回。
// 返回是否真正经历了等待（用于断言"确实命中了屏障"）。
func (b *Barrier) Wait(point BarrierPoint, timeout time.Duration) (armed bool, err error) {
	b.mu.Lock()
	ch, ok := b.gates[point]
	b.mu.Unlock()
	if !ok {
		return false, nil
	}
	select {
	case <-ch:
		return true, nil
	case <-time.After(timeout):
		return true, fmt.Errorf("barrier %s 等待超时（%s），屏障未按预期释放", point, timeout)
	}
}

// Release 放行指定边界点；重复调用安全（幂等）。
func (b *Barrier) Release(point BarrierPoint) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.gates[point]; ok {
		select {
		case <-ch: // 已放行
		default:
			close(ch)
		}
		delete(b.gates, point)
	}
}

// ReleaseAll 放行所有仍未放行的边界点（测试清理）。
func (b *Barrier) ReleaseAll() {
	b.mu.Lock()
	points := make([]BarrierPoint, 0, len(b.gates))
	for p := range b.gates {
		points = append(points, p)
	}
	b.mu.Unlock()
	for _, p := range points {
		b.Release(p)
	}
}

// Armed 返回按 Arm 顺序记录的边界点（用于断言注入点顺序）。
func (b *Barrier) Armed() []BarrierPoint {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]BarrierPoint(nil), b.armLog...)
}

// ---------------------------------------------------------------------------
// ProviderLedger：受控提供者的独立账本
// ---------------------------------------------------------------------------

// LedgerCall 是账本中一次"收到请求"的记录。
type LedgerCall struct {
	Seq         int64     // 账本内单调序号（接收顺序）
	OperationID string    // 逻辑操作标识（幂等键）
	Attempt     int       // 第几次尝试
	Kind        string    // start/get/cancel/query
	PayloadHash string    // 参数摘要（同 ID 不同内容可区分）
	ReceivedAt  time.Time // 收到时间（账本时钟）
}

// LedgerEffect 是一次"实际生效"的记录，与收到请求分开保存。
type LedgerEffect struct {
	Seq         int64
	OperationID string
	Attempt     int
	Applied     bool      // 是否真正生效
	Note        string    // 生效说明（部分生效/冲突等）
	AppliedAt   time.Time
}

// ProviderLedger 独立于 ClusBot 数据库保存提供者的接收与生效账本。
// 同一请求收到两次但去重只生效一次时，账本仍能暴露两次调用，防止掩盖
// 违规重发（P0-04.5）。
type ProviderLedger struct {
	mu      sync.Mutex
	clock   Clock
	calls   []LedgerCall
	effects []LedgerEffect
	// Jobs 记录异步作业状态，按 operationID+attempt 索引。
	Jobs    map[string]*LedgerJob
	// 能力配置：分别声明查询/幂等/fencing/取消支持（P0-04.3）。
	SupportsQuery   bool
	SupportsIdem    bool
	SupportsFence   bool
	SupportsCancel  bool
}

// LedgerJob 是一个异步作业的账本状态。
type LedgerJob struct {
	OperationID string
	Attempt     int
	Status      string // accepted/running/succeeded/failed/cancelled/unknown
	ExternalID  string
	StartedAt   time.Time
	UpdatedAt   time.Time
}

// NewProviderLedger 创建带独立时钟的账本。
func NewProviderLedger(clock Clock) *ProviderLedger {
	return &ProviderLedger{clock: clock, Jobs: map[string]*LedgerJob{}}
}

// jobKey 生成作业索引键。
func jobKey(operationID string, attempt int) string {
	return fmt.Sprintf("%s#%d", operationID, attempt)
}

// RecordCall 记录一次收到的请求，返回账本内序号。
func (l *ProviderLedger) RecordCall(operationID string, attempt int, kind, payloadHash string) LedgerCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	call := LedgerCall{
		Seq:         int64(len(l.calls) + 1),
		OperationID: operationID,
		Attempt:     attempt,
		Kind:        kind,
		PayloadHash: payloadHash,
		ReceivedAt:  l.clock.Now(),
	}
	l.calls = append(l.calls, call)
	return call
}

// RecordEffect 记录一次实际生效，与收到请求分开。
func (l *ProviderLedger) RecordEffect(operationID string, attempt int, applied bool, note string) LedgerEffect {
	l.mu.Lock()
	defer l.mu.Unlock()
	effect := LedgerEffect{
		Seq:         int64(len(l.effects) + 1),
		OperationID: operationID,
		Attempt:     attempt,
		Applied:     applied,
		Note:        note,
		AppliedAt:   l.clock.Now(),
	}
	l.effects = append(l.effects, effect)
	return effect
}

// StartJob 登记一个异步作业。
func (l *ProviderLedger) StartJob(operationID string, attempt int, externalID string) *LedgerJob {
	l.mu.Lock()
	defer l.mu.Unlock()
	job := &LedgerJob{
		OperationID: operationID,
		Attempt:     attempt,
		Status:      "accepted",
		ExternalID:  externalID,
		StartedAt:   l.clock.Now(),
		UpdatedAt:   l.clock.Now(),
	}
	l.Jobs[jobKey(operationID, attempt)] = job
	return job
}

// Calls 返回全部收到请求的副本（按接收顺序）。
func (l *ProviderLedger) Calls() []LedgerCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]LedgerCall(nil), l.calls...)
}

// Effects 返回全部生效记录的副本。
func (l *ProviderLedger) Effects() []LedgerEffect {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]LedgerEffect(nil), l.effects...)
}

// CallCount 返回指定操作/类型的收到次数。
func (l *ProviderLedger) CallCount(operationID, kind string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.calls {
		if c.OperationID == operationID && (kind == "" || c.Kind == kind) {
			n++
		}
	}
	return n
}

// EffectCount 返回指定操作的真正生效次数。
func (l *ProviderLedger) EffectCount(operationID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.effects {
		if e.OperationID == operationID && e.Applied {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// 数据库快照 + 外部账本联合断言（P0-04.5）
// ---------------------------------------------------------------------------

// DBAssertion 是从数据库读取的一组可断言事实。
type DBAssertion struct {
	TaskStatus       map[string]string // task_id → status
	RunStatus        map[string]string // run_id → status
	PublicationOpen  int               // 开放发布数
	UnknownReads     int               // 未确认未知读取数
	QueuedRuns       int               // 排队 Run 数
}

// SnapshotDB 读取数据库当前可断言事实。
func SnapshotDB(db *sql.DB) (*DBAssertion, error) {
	a := &DBAssertion{TaskStatus: map[string]string{}, RunStatus: map[string]string{}}
	rows, err := db.Query("SELECT id,status FROM tasks")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			rows.Close()
			return nil, err
		}
		a.TaskStatus[id] = status
	}
	rows.Close()
	rows, err = db.Query("SELECT id,status FROM runs")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			rows.Close()
			return nil, err
		}
		a.RunStatus[id] = status
	}
	rows.Close()
	for query, dst := range map[string]*int{
		"SELECT COUNT(*) FROM publications WHERE status IN ('pending','dispatched','unknown')": &a.PublicationOpen,
		"SELECT COUNT(*) FROM run_reads WHERE status='unknown' AND acknowledged_at=0":         &a.UnknownReads,
		"SELECT COUNT(*) FROM runs WHERE status='queued'":                                     &a.QueuedRuns,
	} {
		if err := db.QueryRow(query).Scan(dst); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// AssertJoint 联合断言数据库快照与外部账本，防止单边结论。
// 规则示例：数据库认为"未派发"时账本不能有该操作的生效记录；
// 数据库认为"已派发成功"时账本必须有对应生效（除非显式标记未知）。
func AssertJoint(t *testing.T, db *DBAssertion, ledger *ProviderLedger, operationID string) {
	t.Helper()
	effects := ledger.EffectCount(operationID)
	calls := ledger.CallCount(operationID, "")
	t.Logf("joint assertion: operation=%s ledger calls=%d effects=%d", operationID, calls, effects)
	// 账本暴露的调用次数不得少于生效次数（防止掩盖违规重发）。
	if calls < effects {
		t.Errorf("operation %s: ledger effects=%d exceed calls=%d (违规重发被掩盖)", operationID, effects, calls)
	}
}

// ---------------------------------------------------------------------------
// 子进程测试帮助（P0-04.4 工作者终止与重连）
// ---------------------------------------------------------------------------

// WorkerCrashKind 区分进程崩溃与硬件断电（P0-04.2 反例：没有相应环境不能
// 写断电已验证）。
type WorkerCrashKind string

const (
	// CrashProcess：进程崩溃（可模拟）。
	CrashProcess WorkerCrashKind = "process"
	// CrashPower：硬件断电（本机测试环境不具备，标记为未验证）。
	CrashPower WorkerCrashKind = "power"
)

// RecordCrash 记录一次崩溃注入及其类型；power 类型在本机环境标记为未验证。
func RecordCrash(t *testing.T, kind WorkerCrashKind, detail string) {
	t.Helper()
	if kind == CrashPower {
		t.Logf("POWER-OFF crash NOT verified in this environment: %s", detail)
		return
	}
	t.Logf("process crash injected: %s", detail)
}

// ---------------------------------------------------------------------------
// 受控提供者（P0-04.3）：把账本与故障注入接到 ClusBot Provider
// ---------------------------------------------------------------------------

// ControlledProvider 实现 work.Provider，把每次模型调用记录到账本，
// 并按配置注入响应丢失、重复、乱序与同 ID 不同内容。模型输出不作为测试
// 真值——真正的判据是账本与数据库快照。
type ControlledProvider struct {
	Ledger *ProviderLedger
	// Respond 是按调用序返回内容的脚本；key 用于区分同 ID 不同内容。
	Respond func(call LedgerCall) (string, error)
	// DropResponse 注入响应丢失：第 N 次调用（1 起）不返回内容。
	DropResponse map[int]bool
	// Duplicate 注入重复：第 N 次调用的结果被记录两次（模拟重投）。
	Duplicate map[int]bool

	mu       sync.Mutex
	sequence int
	seen     map[string]int // payloadHash → 首次序号（同 ID 不同内容检测）
}

// NewControlledProvider 创建受控提供者。
func NewControlledProvider(ledger *ProviderLedger) *ControlledProvider {
	return &ControlledProvider{Ledger: ledger, seen: map[string]int{}}
}

// Stream 实现 Provider 接口：记录到账本后按脚本返回。
func (p *ControlledProvider) Stream(ctx context.Context, messages []Message, _ bool, _ int, emit func(string) error) (*TokenUsage, error) {
	p.mu.Lock()
	p.sequence++
	seq := p.sequence
	p.mu.Unlock()

	// 计算本次调用的参数摘要（同 ID 不同内容可区分）。
	payloadHash := digest(fmt.Sprintf("%v", messages))
	call := p.Ledger.RecordCall("model", seq, "stream", payloadHash)

	// 同 ID 不同内容检测：记录首次序号，后续同 payload 复用。
	p.mu.Lock()
	if _, ok := p.seen[payloadHash]; !ok {
		p.seen[payloadHash] = seq
	}
	p.mu.Unlock()

	if p.DropResponse[seq] {
		return nil, errors.New("受控注入：响应丢失")
	}
	text := "受控回复"
	if p.Respond != nil {
		var err error
		text, err = p.Respond(call)
		if err != nil {
			return nil, err
		}
	}
	if p.Duplicate[seq] {
		// 模拟重复投递：账本额外记录一次接收，但只生效一次。
		p.Ledger.RecordCall("model", seq, "stream", payloadHash)
	}
	if err := emit(text); err != nil {
		return nil, err
	}
	p.Ledger.RecordEffect("model", seq, true, "回复已生效")
	input, output := int64(len(text)/2+1), int64(len(text)+1)
	return &TokenUsage{InputTokens: &input, OutputTokens: &output}, nil
}
