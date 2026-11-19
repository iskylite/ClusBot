package work

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// SubmitBenchmarkCommand is the explicit, model-independent route for a
// published profile. Free-form chat never becomes an implicit benchmark write.
func (s *Store) SubmitBenchmarkCommand(ctx context.Context, key, actor, conversation, text, scope string, provider BenchmarkProvider) (Run, bool, error) {
	fields := strings.Fields(text)
	if len(fields) == 0 || (fields[0] != "/benchmark" && fields[0] != "/benchmark-status") {
		return Run{}, false, nil
	}
	if len(fields) != 2 {
		return Run{}, true, fmt.Errorf("%w：用 /benchmark 方案@版本 或 /benchmark-status 任务ID", ErrBenchmarkInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, true, err
	}
	defer tx.Rollback()
	stamp := now()
	fingerprint := submissionFingerprint(text, scope, conversation, false)
	previous, err := previousSubmission(tx, key, fingerprint)
	if err != nil || previous.ID != "" {
		return previous, true, err
	}
	if conversation != "" {
		if err = checkConversationSubmission(tx, conversation); err != nil {
			return Run{}, true, err
		}
	}
	task := fields[1]
	var response string
	if fields[0] == "/benchmark" {
		profile, v, ok := strings.Cut(fields[1], "@")
		version, e := strconv.Atoi(v)
		if !ok || e != nil || version < 1 {
			return Run{}, true, ErrBenchmarkInput
		}
		task, err = submitBenchmarkRunTx(tx, actor, BenchmarkRunRequest{RequestKey: "chat:" + key, Profile: profile, Version: version}, provider, stamp)
		if err != nil {
			return Run{}, true, err
		}
		response = fmt.Sprintf("已受理基准方案 %s@%d。这里只确认任务创建，不代表外部作业已启动。\n\n查看进度：`/benchmark-status %s`", profile, version, task)
	} else {
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
