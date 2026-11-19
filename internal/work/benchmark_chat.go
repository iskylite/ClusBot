package work

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SubmitBenchmarkCommand handles explicit slash commands and narrowly matched
// direct orders. Other messages stay on the ordinary chat path.
func (s *Store) SubmitBenchmarkCommand(ctx context.Context, key, actor, conversation, text, scope string, provider BenchmarkProvider) (Run, bool, error) {
	fields := strings.Fields(text)
	explicit := len(fields) > 0 && (fields[0] == "/benchmark" || fields[0] == "/benchmark-status")
	target := ""
	if !explicit {
		target = benchmarkOrderTargetFromText(text)
		if target == "" {
			return Run{}, false, nil
		}
	} else if len(fields) != 2 {
		return Run{}, true, fmt.Errorf("%w：用 /benchmark 方案@版本 或 /benchmark-status 任务ID", ErrBenchmarkInput)
	}
	if actor == "" {
		return Run{}, true, ErrAuthorizationDenied
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, true, err
	}
	defer tx.Rollback()
	stamp := now()
	fingerprint := submissionFingerprint(text, scope, conversation, false)
	// Check the owner before replaying a receipt. A request previously accepted
	// as ordinary chat must not be promoted into an order on retry.
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT principal
FROM inbox_events WHERE source='chat' AND event_id=?`, key).Scan(&owner); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Run{}, true, err
	}
	if owner != "" && owner != actor {
		return Run{}, true, ErrConflict
	}
	previous, err := previousSubmission(tx, key, fingerprint)
	if err != nil {
		return Run{}, true, err
	}
	if previous.ID != "" {
		if owner == "" {
			if explicit {
				return Run{}, true, ErrConflict
			}
			return Run{}, false, nil
		}
		return previous, true, nil
	}
	if owner != "" {
		if !explicit {
			return Run{}, false, nil
		}
		return Run{}, true, ErrConflict
	}
	if conversation != "" {
		if err = checkConversationSubmission(tx, conversation); err != nil {
			return Run{}, true, err
		}
	}
	var task, response string
	if target != "" || fields[0] == "/benchmark" {
		var profile string
		var version int
		if target != "" {
			err = tx.QueryRowContext(ctx, `SELECT profile_key,profile_version FROM benchmark_chat_targets WHERE target=?`, target).Scan(&profile, &version)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				response = fmt.Sprintf("没有找到为 %s 启用的全套基准方案，没有创建测试任务。请先绑定已发布方案，或明确指定要跑的方案和版本。", target)
			case err != nil:
				return Run{}, true, err
			}
		} else {
			var v string
			var ok bool
			profile, v, ok = strings.Cut(fields[1], "@")
			version, err = strconv.Atoi(v)
			if !ok || err != nil || version < 1 {
				return Run{}, true, ErrBenchmarkInput
			}
		}
		if response == "" {
			task, err = submitBenchmarkRunTx(tx, actor, BenchmarkRunRequest{RequestKey: "chat:" + key, Profile: profile, Version: version}, provider, stamp)
			if err != nil {
				return Run{}, true, err
			}
			if target == "" {
				response = fmt.Sprintf("已受理基准方案 %s@%d。", profile, version)
			} else {
				response = fmt.Sprintf("已受理 %s 的全套基准，使用已发布方案 %s@%d。", target, profile, version)
			}
			response += fmt.Sprintf("这里只确认任务创建，不代表外部作业已启动。\n\n查看进度：`/benchmark-status %s`", task)
		}
	} else {
		task = fields[1]
		var state string
		var total, verified, incomplete int
		err = tx.QueryRow(`SELECT t.state,count(*),sum(c.phase='verified'),sum(c.phase IN ('incomplete','skipped'))
 FROM benchmark_runs r JOIN task_states t ON t.task_id=r.task_id JOIN benchmark_cases c ON c.task_id=r.task_id
 WHERE r.task_id=? AND r.actor=? GROUP BY r.task_id`, task, actor).Scan(&state, &total, &verified, &incomplete)
		if err != nil {
			return Run{}, true, err
		}
		stateLabel := map[string]string{"pending": "待执行", "running": "执行或对账中", "finalizing": "正在停止并核对在途作业",
			"succeeded": "所有必测项已核实", "failed": "存在未核实或未执行的必测项", "cancelled": "已停止"}[state]
		if stateLabel == "" {
			stateLabel = state
		}
		response = fmt.Sprintf("任务 `%s`：%s。已核实 %d/%d 项；%d 项未核实或未执行。性能达标与跑齐分开记录。", task, stateLabel, verified, total, incomplete)
	}
	if _, err = admitChatInputWithFingerprint(tx, key, actor, text, fingerprint, "", stamp); err != nil {
		return Run{}, true, err
	}
	if conversation == "" {
		conversation = id()
		if _, err = tx.Exec("INSERT INTO conversations(id,title,created_at) VALUES(?,?,?)", conversation, "基准测试", stamp); err != nil {
			return Run{}, true, err
		}
	}
	r := Run{ID: id(), ConversationID: conversation, Status: "completed", Response: response}
	if _, err = tx.Exec("INSERT INTO runs(id,conversation_id,status,response,created_at,usage_tracked) VALUES(?,?,?,?,?,1)", r.ID, conversation, r.Status, response, stamp); err != nil {
		return Run{}, true, err
	}
	msg, err := tx.Exec("INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'user',?)", conversation, r.ID, text)
	if err != nil {
		return Run{}, true, err
	}
	seq, err := msg.LastInsertId()
	if err != nil {
		return Run{}, true, err
	}
	if err = recordInputEvent(tx, seq, key); err != nil {
		return Run{}, true, err
	}
	if _, err = tx.Exec("INSERT INTO input_dispositions VALUES(?,?,0,'completed',?)", seq, r.ID, stamp); err != nil {
		return Run{}, true, err
	}
	if _, err = tx.Exec("INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'assistant',?)", conversation, r.ID, response); err != nil {
		return Run{}, true, err
	}
	if _, err = tx.Exec("INSERT INTO submissions VALUES(?,?,?)", key, fingerprint, r.ID); err != nil {
		return Run{}, true, err
	}
	if err = linkAdmissionToRun(tx, key, r.ID, task, stamp); err != nil {
		return Run{}, true, err
	}
	return r, true, tx.Commit()
}
