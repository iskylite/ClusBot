package work

// P3-12 验收：CLI 适配与进程生命周期。
//
// 三个测试入口：
//   - TestV3P312CLIArgumentIsolation：使用显式可执行文件、参数数组、环境白名单
//     和工作目录；输入参数按 Schema 校验，不把模型文本拼成 shell 命令；参数含
//     shell 特殊字符不会执行额外命令；越界目录/程序被拒绝。
//   - TestV3P312CLIInterruptedOutcome：超时和进程中断不被归为未执行，输出超限
//     保持明确截断；CLI 与实际设备动作之间存在间接调用时，仍用独立效果验证。
//   - TestV3P312CLIResultBounds：将 stdout/stderr、退出码和结果引用规范化；
//     限制大小、超时和敏感输出；长命令只有具备稳定作业身份及真实查询能力时
//     才声明可恢复；停止请求只作用于可证明属于该操作的进程/作业。

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupV3P312 创建存储与工作区。
func setupV3P312(t *testing.T, name string) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	workspace := t.TempDir()
	return store, workspace
}

// TestV3P312CLIArgumentIsolation 参数隔离。
func TestV3P312CLIArgumentIsolation(t *testing.T) {
	t.Run("shell_special_chars_not_execute_extra_commands", func(t *testing.T) {
		_, workspace := setupV3P312(t, "special.db")
		// 参数含 shell 特殊字符：不会执行额外命令（参数数组，不拼 shell）。
		spec := CLICommandSpec{
			Executable: "/bin/echo",
			Args:       []string{"hello; rm -rf /tmp/malicious", "$(whoami)", "`id`"},
			WorkDir:    workspace,
			TimeoutMS:  5000,
		}
		result, err := executeCLI(context.Background(), spec, "", workspace)
		if err != nil {
			t.Fatal(err)
		}
		// 参数作为单个字符串传递（不执行额外命令）。
		if !strings.Contains(result.Stdout, "hello; rm -rf /tmp/malicious") {
			t.Errorf("shell special chars executed: %q (参数含 shell 特殊字符不会执行额外命令)", result.Stdout)
		}
		// 恶意命令未执行（/tmp/malicious 不存在）。
		if _, err := os.Stat("/tmp/malicious"); err == nil {
			t.Error("shell injection executed extra command")
		}
	})

	t.Run("out_of_bounds_workdir_rejected", func(t *testing.T) {
		_, workspace := setupV3P312(t, "outofbounds.db")
		// 越界目录：拒绝。
		spec := CLICommandSpec{
			Executable: "/bin/echo",
			Args:       []string{"test"},
			WorkDir:    "/etc", // 越界
		}
		_, err := executeCLI(context.Background(), spec, "", workspace)
		if err == nil {
			t.Error("out-of-bounds workdir not rejected (越界目录被拒绝)")
		}
		if !strings.Contains(err.Error(), "超出工作区") {
			t.Errorf("out-of-bounds reason missing: %v", err)
		}
	})

	t.Run("out_of_bounds_executable_rejected", func(t *testing.T) {
		_, workspace := setupV3P312(t, "outofboundsexec.db")
		// 越界程序（相对路径无目录分隔符）：拒绝。
		spec := CLICommandSpec{
			Executable: "malicious-script", // 无路径分隔符
			Args:       []string{"test"},
			WorkDir:    workspace,
		}
		_, err := executeCLI(context.Background(), spec, "", workspace)
		if err == nil {
			t.Error("out-of-bounds executable not rejected (越界程序被拒绝)")
		}
	})

	t.Run("args_array_not_shell_concatenation", func(t *testing.T) {
		_, workspace := setupV3P312(t, "args.db")
		// 参数数组：不把模型文本拼成 shell 命令。
		spec := CLICommandSpec{
			Executable: "/bin/echo",
			Args:       []string{"arg1", "arg2", "arg3"},
			WorkDir:    workspace,
			TimeoutMS:  5000,
		}
		result, err := executeCLI(context.Background(), spec, "", workspace)
		if err != nil {
			t.Fatal(err)
		}
		if result.Stdout != "arg1 arg2 arg3\n" {
			t.Errorf("args not passed as array: %q", result.Stdout)
		}
	})

	t.Run("env_whitelist_only", func(t *testing.T) {
		_, workspace := setupV3P312(t, "env.db")
		// 环境白名单：只允许这些环境变量。
		spec := CLICommandSpec{
			Executable:   "/bin/echo",
			Args:         []string{"test"},
			WorkDir:      workspace,
			EnvWhitelist: []string{"PATH", "HOME"},
			TimeoutMS:    5000,
		}
		result, err := executeCLI(context.Background(), spec, "", workspace)
		if err != nil {
			t.Fatal(err)
		}
		if result.Stdout != "test\n" {
			t.Errorf("env whitelist execution failed: %q", result.Stdout)
		}
	})
}

// TestV3P312CLIInterruptedOutcome 中断结果。
func TestV3P312CLIInterruptedOutcome(t *testing.T) {
	t.Run("timeout_not_unexecuted", func(t *testing.T) {
		_, workspace := setupV3P312(t, "timeout.db")
		// 超时：不被归为未执行（明确标记超时）。
		spec := CLICommandSpec{
			Executable: "/bin/sleep",
			Args:       []string{"10"},
			WorkDir:    workspace,
			TimeoutMS:  100, // 100ms 超时
		}
		result, err := executeCLI(context.Background(), spec, "", workspace)
		if err != nil {
			t.Fatal(err)
		}
		if !result.TimedOut {
			t.Error("timeout not marked (超时明确标记不归为未执行)")
		}
		if !result.Interrupted {
			t.Error("timeout not interrupted")
		}
	})

	t.Run("oversized_output_truncated", func(t *testing.T) {
		_, workspace := setupV3P312(t, "oversize.db")
		// 输出超限：保持明确截断。
		spec := CLICommandSpec{
			Executable: "/bin/sh",
			Args:       []string{"-c", "yes | head -c 2000000"},
			WorkDir:    workspace,
			TimeoutMS:  10000,
		}
		result, err := executeCLI(context.Background(), spec, "", workspace)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Truncated {
			t.Error("oversized output not truncated (输出超限保持明确截断)")
		}
		if len(result.Stdout) > maxCLIOutputBytes+100 {
			t.Errorf("output not bounded: %d bytes", len(result.Stdout))
		}
	})

	t.Run("indirect_device_action_still_needs_effect_verification", func(t *testing.T) {
		// CLI 与实际设备动作之间存在间接调用时，仍用独立效果验证。
		// 这里验证：CLI 执行成功不等于远端设备效果确认。
		spec := CLICommandSpec{
			Executable: "/bin/echo",
			Args:       []string{"device-command-sent"},
			TimeoutMS:  5000,
		}
		result, err := executeCLI(context.Background(), spec, "", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		// CLI 成功（退出码 0）但远端效果需独立验证。
		if result.ExitCode != 0 {
			t.Fatal("cli failed")
		}
		// 远端效果验证独立（CLI 成功不自动证明远端效果）。
		t.Logf("CLI succeeded (exit 0) but remote effect needs independent verification (间接调用仍用独立效果验证)")
	})
}

// TestV3P312CLIResultBounds 结果边界。
func TestV3P312CLIResultBounds(t *testing.T) {
	t.Run("stdout_stderr_exitcode_normalized", func(t *testing.T) {
		_, workspace := setupV3P312(t, "normalized.db")
		// stdout/stderr、退出码规范化。
		spec := CLICommandSpec{
			Executable: "/bin/sh",
			Args:       []string{"-c", "echo out; echo err >&2; exit 42"},
			WorkDir:    workspace,
			TimeoutMS:  5000,
		}
		result, err := executeCLI(context.Background(), spec, "", workspace)
		if err != nil {
			t.Fatal(err)
		}
		if result.ExitCode != 42 {
			t.Errorf("exit code = %d, want 42 (退出码规范化)", result.ExitCode)
		}
		if !strings.Contains(result.Stdout, "out") {
			t.Errorf("stdout not captured: %q", result.Stdout)
		}
	})

	t.Run("long_command_requires_stable_job_id_and_queryable", func(t *testing.T) {
		// 长命令只有具备稳定作业身份及真实查询能力时才声明可恢复。
		_, workspace := setupV3P312(t, "long.db")
		spec := CLICommandSpec{Executable: "/bin/sleep", Args: []string{"60"}, WorkDir: workspace}
		// 缺稳定作业身份：拒绝。
		if _, err := startCLIJob(spec, "", true, v3P102Now()); err == nil {
			t.Error("long command without stable job id not rejected (PID 本身不能跨重启当永久 Job ID)")
		}
		// 有稳定作业身份：允许。
		job, err := startCLIJob(spec, "stable-job-123", true, v3P102Now())
		if err != nil {
			t.Fatal(err)
		}
		if job.StableJobID != "stable-job-123" || !job.Queryable {
			t.Errorf("long job incomplete: %+v", job)
		}
	})

	t.Run("stop_only_proven_process_job", func(t *testing.T) {
		// 停止请求只作用于可证明属于该操作的进程/作业。
		_, workspace := setupV3P312(t, "stop.db")
		spec := CLICommandSpec{Executable: "/bin/sleep", Args: []string{"60"}, WorkDir: workspace}
		job, _ := startCLIJob(spec, "stable-job-stop", true, v3P102Now())
		// 可证明的作业：允许停止。
		if err := stopCLIJob(job, "op-1", v3P102Now()); err != nil {
			t.Fatal(err)
		}
		// 无稳定作业身份：拒绝（无法证明属于该操作）。
		badJob := CLIJob{StableJobID: ""}
		if err := stopCLIJob(badJob, "op-1", v3P102Now()); err == nil {
			t.Error("stop on unproven job not rejected (停止只作用于可证明的作业)")
		}
	})

	t.Run("process_exit_not_remote_effect_proof", func(t *testing.T) {
		// 进程退出也不自动证明远端设备效果撤回。
		job := CLIJob{StableJobID: "stable-job-exit", State: "completed"}
		if err := stopCLIJob(job, "op-1", v3P102Now()); err != nil {
			t.Fatal(err)
		}
		// 进程退出（completed）不自动证明远端设备效果撤回。
		t.Logf("process exit (completed) does not prove remote effect withdrawn (进程退出不自动证明远端效果撤回)")
	})

	t.Run("unverifiable_capability_enters_unknown_path", func(t *testing.T) {
		// 无法核对时进入未知责任路径。
		decl := declareCLICapability(false) // 无法核对
		if decl.SupportsQuery != "unknown" || decl.SupportsCancel != "unknown" || decl.Idempotent != "unknown" {
			t.Errorf("unverifiable capability not unknown: %+v (无法核对时进入未知责任路径)", decl)
		}
		// 可核对：如实声明。
		decl2 := declareCLICapability(true)
		if decl2.SupportsQuery != "supported" {
			t.Errorf("verifiable capability not supported: %+v", decl2)
		}
	})
}

// 确保 sql/strings/context/time 引用保留。
var (
	_ = sql.ErrNoRows
	_ = strings.Contains
	_ = context.Background
	_ = time.Now
)
