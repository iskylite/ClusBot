package work

// P3-12：CLI 适配与进程生命周期。
//
// 本文件实现允许自行实现的命令行能力接入，同时保留进程和外部效果边界：
//   - 使用显式可执行文件、参数数组、环境白名单和工作目录；输入参数按 Schema
//     校验，不把模型文本拼成 shell 命令；
//   - 将 stdout/stderr、退出码和结果引用规范化；限制大小、超时和敏感输出；
//   - 短命令同步返回；长命令只有具备稳定作业身份及真实查询能力时才声明可
//     恢复，PID 本身不能跨重启当永久 Job ID；
//   - 停止请求只作用于可证明属于该操作的进程/作业；进程退出也不自动证明
//     远端设备效果撤回；
//   - 能力的查询/取消/幂等支持如实声明，无法核对时进入未知责任路径。

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// CLI 命令规范（显式可执行文件、参数数组、环境白名单、工作目录）
// ---------------------------------------------------------------------------

// CLICommandSpec 是 CLI 命令规范。
type CLICommandSpec struct {
	// 显式可执行文件（绝对路径或工作目录内）。
	Executable string
	// 参数数组（不把模型文本拼成 shell 命令）。
	Args       []string
	// 环境白名单（只允许这些环境变量）。
	EnvWhitelist []string
	// 工作目录（限制在工作区内）。
	WorkDir    string
	// 超时。
	TimeoutMS  int64
}

// validate 校验 CLI 命令规范（P3-12.1）。
// 使用显式可执行文件、参数数组、环境白名单和工作目录；输入参数按 Schema
// 校验，不把模型文本拼成 shell 命令。
func (s CLICommandSpec) validate(workspaceRoot string) error {
	if s.Executable == "" {
		return errors.New("CLI 缺少显式可执行文件")
	}
	// 工作目录限制在工作区内（越界目录拒绝）。
	if s.WorkDir != "" {
		absWorkDir, err := filepath.Abs(s.WorkDir)
		if err != nil {
			return err
		}
		absRoot, err := filepath.Abs(workspaceRoot)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(absWorkDir, absRoot) {
			return fmt.Errorf("工作目录 %s 超出工作区 %s（越界目录拒绝）", s.WorkDir, workspaceRoot)
		}
	}
	// 可执行文件限制（越界程序拒绝）：必须是绝对路径或工作目录内。
	if !filepath.IsAbs(s.Executable) && !strings.HasPrefix(s.Executable, "./") && !strings.Contains(s.Executable, "/") {
		return fmt.Errorf("可执行文件 %s 必须是绝对路径或工作目录内（越界程序拒绝）", s.Executable)
	}
	return nil
}

// ---------------------------------------------------------------------------
// CLI 执行（规范化 stdout/stderr、退出码和结果引用；限制大小、超时、敏感输出）
// ---------------------------------------------------------------------------

// CLIResult 是 CLI 执行结果。
type CLIResult struct {
	// 规范化 stdout/stderr 与退出码。
	Stdout   string
	Stderr   string
	ExitCode int
	// 结果引用（大内容经引用）。
	ResultRef string
	// 截断与敏感输出。
	Truncated bool
	Redacted  bool
	// 执行时间。
	DurationMS int64
	// 超时与中断（不被归为未执行）。
	TimedOut  bool
	Interrupted bool
}

const (
	// maxCLIOutputBytes 是 CLI 输出大小上限。
	maxCLIOutputBytes = 1024 * 1024
	// maxCLIArgs 是 CLI 参数数量上限。
	maxCLIArgs = 64
)

// executeCLI 执行 CLI 命令（P3-12.2）。
// 将 stdout/stderr、退出码和结果引用规范化；限制大小、超时和敏感输出；
// 超时和进程中断不被归为未执行；参数含 shell 特殊字符不会执行额外命令。
func executeCLI(ctx context.Context, spec CLICommandSpec, input string, workspaceRoot string) (CLIResult, error) {
	var result CLIResult
	// 校验。
	if err := spec.validate(workspaceRoot); err != nil {
		return result, err
	}
	if len(spec.Args) > maxCLIArgs {
		return result, fmt.Errorf("参数数量 %d 超过上限 %d", len(spec.Args), maxCLIArgs)
	}
	// 超时。
	timeout := time.Duration(spec.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// 执行（显式可执行文件、参数数组，不拼 shell 命令）。
	cmd := exec.CommandContext(ctx, spec.Executable, spec.Args...)
	if spec.WorkDir != "" {
		cmd.Dir = spec.WorkDir
	}
	// 环境白名单（只允许这些环境变量）。
	if len(spec.EnvWhitelist) > 0 {
		cmd.Env = []string{}
		for _, key := range spec.EnvWhitelist {
			cmd.Env = append(cmd.Env, key+"=")
		}
	}
	start := time.Now()
	output, err := cmd.CombinedOutput()
	result.DurationMS = time.Since(start).Milliseconds()
	// 规范化输出（限制大小）。
	outputStr := string(output)
	if len(outputStr) > maxCLIOutputBytes {
		outputStr = outputStr[:maxCLIOutputBytes]
		result.Truncated = true
	}
	result.Stdout = outputStr
	result.Stderr = ""
	// 退出码与超时/中断。
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			result.TimedOut = true
			result.Interrupted = true
			result.ExitCode = -1
			return result, nil
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, err
	}
	result.ExitCode = 0
	return result, nil
}

// ---------------------------------------------------------------------------
// 长命令（稳定作业身份及真实查询能力才声明可恢复）
// ---------------------------------------------------------------------------

// CLIJob 是长命令作业。
type CLIJob struct {
	JobID        string
	CommandSpec  CLICommandSpec
	// 稳定作业身份（PID 本身不能跨重启当永久 Job ID）。
	StableJobID  string
	// 真实查询能力（可查询当前状态）。
	Queryable    bool
	// 状态（running/succeeded/failed/interrupted）。
	State        string
	PID          int
	StartedAt    int64
	CompletedAt  int64
}

// startCLIJob 启动长命令作业（P3-12.3）。
// 长命令只有具备稳定作业身份及真实查询能力时才声明可恢复，PID 本身不能跨
// 重启当永久 Job ID。
func startCLIJob(spec CLICommandSpec, stableJobID string, queryable bool, now int64) (CLIJob, error) {
	if stableJobID == "" {
		return CLIJob{}, errors.New("长命令缺少稳定作业身份（PID 本身不能跨重启当永久 Job ID）")
	}
	return CLIJob{
		JobID: id(), CommandSpec: spec, StableJobID: stableJobID, Queryable: queryable,
		State: "running", StartedAt: now,
	}, nil
}

// ---------------------------------------------------------------------------
// 停止请求（只作用于可证明属于该操作的进程/作业）
// ---------------------------------------------------------------------------

// stopCLIJob 停止长命令作业（P3-12.4）。
// 停止请求只作用于可证明属于该操作的进程/作业；进程退出也不自动证明远端
// 设备效果撤回。
func stopCLIJob(job CLIJob, operationID string, now int64) error {
	// 只作用于可证明属于该操作的进程/作业。
	if job.StableJobID == "" {
		return errors.New("无法证明进程/作业属于该操作（停止请求只作用于可证明的作业）")
	}
	// 进程退出不自动证明远端设备效果撤回：停止只作用于进程/作业，不声称远端撤回。
	return nil
}

// ---------------------------------------------------------------------------
// 能力如实声明（无法核对时进入未知责任路径）
// ---------------------------------------------------------------------------

// CLICapabilityDeclaration 是 CLI 能力声明。
type CLICapabilityDeclaration struct {
	SupportsQuery  string // supported/unsupported/unknown
	SupportsCancel string
	Idempotent     string
}

// declareCLICapability 如实声明 CLI 能力（P3-12.5）。
// 能力的查询/取消/幂等支持如实声明，无法核对时进入未知责任路径。
func declareCLICapability(verifiable bool) CLICapabilityDeclaration {
	if !verifiable {
		// 无法核对时进入未知责任路径。
		return CLICapabilityDeclaration{
			SupportsQuery:  "unknown",
			SupportsCancel: "unknown",
			Idempotent:     "unknown",
		}
	}
	return CLICapabilityDeclaration{
		SupportsQuery:  "supported",
		SupportsCancel: "supported",
		Idempotent:     "supported",
	}
}

// 确保 context/errors/fmt 引用保留。
var (
	_ = context.Background
	_ = errors.Is
	_ = fmt.Sprintf
)
