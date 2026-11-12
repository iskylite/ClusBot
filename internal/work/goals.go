package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrGoal = errors.New("请输入目标、有效的执行时间和次数")

type Goal struct {
	ID        string `json:"id"`
	Target    string `json:"target"`
	Scope     string `json:"scope_label"`
	Criteria  string `json:"acceptance_criteria"`
	Interval  int64  `json:"interval_ms"`
	NextAt    int64  `json:"next_at"`
	ExpiresAt int64  `json:"expires_at"`
	Remaining int    `json:"remaining"`
	Status    string `json:"status"`
	Revision  int    `json:"revision"`
}

const goalColumns = "id,target,scope_label,acceptance_criteria,interval_ms,next_at,expires_at,remaining,status,revision"

func scanGoal(row scanner) (Goal, error) {
	var g Goal
	err := row.Scan(&g.ID, &g.Target, &g.Scope, &g.Criteria, &g.Interval, &g.NextAt, &g.ExpiresAt, &g.Remaining, &g.Status, &g.Revision)
	return g, err
}

func (s *Store) Goals(ctx context.Context) ([]Goal, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+goalColumns+" FROM goals ORDER BY rowid DESC LIMIT 100")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	goals := []Goal{}
	for rows.Next() {
		goal, err := scanGoal(rows)
		if err != nil {
			return nil, err
		}
		goals = append(goals, goal)
	}
	return goals, rows.Err()
}

func (s *Store) CreateGoal(ctx context.Context, requestID string, g Goal) (Goal, error) {
	g.ID, g.Status, g.Revision = digest("goal:" + requestID)[:32], "active", 1
	g.Target, g.Scope, g.Criteria = strings.TrimSpace(g.Target), strings.TrimSpace(g.Scope), strings.TrimSpace(g.Criteria)
	maxDuration := (365 * 24 * time.Hour).Milliseconds()
	if requestID == "" || g.Target == "" || utf8.RuneCountInString(g.Target) > 16000 ||
		utf8.RuneCountInString(g.Scope) > 40 || utf8.RuneCountInString(g.Criteria) > 1000 ||
		g.Interval < time.Hour.Milliseconds() || g.Interval > maxDuration || g.NextAt <= 0 ||
		g.ExpiresAt <= g.NextAt || g.ExpiresAt > now()+maxDuration || g.Remaining < 1 || g.Remaining > 30 {
		return Goal{}, ErrGoal
	}
	payload, err := json.Marshal(g)
	if err != nil {
		return Goal{}, err
	}
	fingerprint := digest(string(payload))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Goal{}, err
	}
	defer tx.Rollback()
	var previous string
	err = tx.QueryRowContext(ctx, "SELECT fingerprint FROM goals WHERE id=?", g.ID).Scan(&previous)
	if err == nil {
		if previous != fingerprint {
			return Goal{}, ErrConflict
		}
		return scanGoal(tx.QueryRowContext(ctx, "SELECT "+goalColumns+" FROM goals WHERE id=?", g.ID))
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Goal{}, err
	}
	if g.ExpiresAt <= now() {
		return Goal{}, ErrGoal
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO goals(fingerprint,"+goalColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?)",
		fingerprint, g.ID, g.Target, g.Scope, g.Criteria, g.Interval, g.NextAt, g.ExpiresAt, g.Remaining, g.Status, g.Revision); err != nil {
		return Goal{}, err
	}
	return g, tx.Commit()
}

func (s *Store) StopGoal(ctx context.Context, id string, revision int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	g, err := scanGoal(tx.QueryRowContext(ctx, "SELECT "+goalColumns+" FROM goals WHERE id=?", id))
	if err != nil {
		return err
	}
	if g.Status == "stopped" && g.Revision == revision+1 {
		return nil
	}
	if g.Revision != revision {
		return ErrTaskRevision
	}
	if _, err = tx.ExecContext(ctx, "UPDATE goals SET status='stopped',revision=revision+1 WHERE id=?", id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE task_followups SET cancelled_at=?
WHERE dispatched_run_id IS NULL AND cancelled_at IS NULL
AND task_id IN (SELECT task_id FROM goal_tasks WHERE goal_id=?)`, now(), id); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.id,t.revision FROM tasks t JOIN goal_tasks g ON g.task_id=t.id
WHERE g.goal_id=? AND t.status='active'`, id)
	if err != nil {
		return err
	}
	var active []Task
	for rows.Next() {
		var task Task
		if err = rows.Scan(&task.ID, &task.Revision); err != nil {
			rows.Close()
			return err
		}
		active = append(active, task)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, task := range active {
		// The transaction upgrades pending pause/stop to the Goal's stop.
		if _, err := tx.ExecContext(ctx, "UPDATE runs SET control='' WHERE task_id=? AND status='running'", task.ID); err != nil {
			return err
		}
		if err := controlTask(tx, task.ID, "stop", task.Revision); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) scheduleGoals(ctx context.Context, stamp int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE goals SET status='expired',revision=revision+1 WHERE status='active' AND expires_at<=?", stamp); err != nil {
		return err
	}
	g, err := scanGoal(tx.QueryRowContext(ctx, `SELECT `+goalColumns+` FROM goals
WHERE status='active' AND next_at<=? AND NOT EXISTS(SELECT 1 FROM goal_tasks x JOIN runs r ON r.task_id=x.task_id
WHERE x.goal_id=goals.id AND r.status IN ('queued','running')) ORDER BY next_at,id LIMIT 1`, stamp))
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	// Coalesce missed periods into one occurrence after a service outage.
	slot := g.NextAt + ((stamp-g.NextAt)/g.Interval)*g.Interval
	run := Run{ID: id(), ConversationID: id(), TaskID: id(), Status: "queued"}
	if _, err := tx.Exec("INSERT INTO conversations(id,title,created_at) VALUES(?,?,?)", run.ConversationID, title(g.Target), stamp); err != nil {
		return err
	}
	if err := insertTask(tx, run, g.Target, g.Scope, Delivery{Criteria: &g.Criteria}); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO runs(id,conversation_id,task_id,status,created_at) VALUES(?,?,?,?,?)",
		run.ID, run.ConversationID, run.TaskID, run.Status, stamp); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO goal_tasks VALUES(?,?,?)", g.ID, slot, run.TaskID); err != nil {
		return err
	}
	result, err := tx.Exec("INSERT INTO messages(conversation_id,run_id,role,text) VALUES(?,?,'user',?)",
		run.ConversationID, run.ID, g.Target)
	if err != nil {
		return err
	}
	message, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if err := recordInputEvent(tx, message, "goal:"+run.ID); err != nil {
		return err
	}
	if err := addEvent(tx, run.ID, "run.state_changed", map[string]string{"status": "queued"}); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE goals SET next_at=?,remaining=remaining-1,status=CASE WHEN remaining=1 THEN 'exhausted' ELSE status END,revision=revision+1 WHERE id=?",
		slot+g.Interval, g.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) runGoalScheduler(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case stamp := <-ticker.C:
			if err := s.scheduleGoals(ctx, stamp.UnixMilli()); err != nil && ctx.Err() == nil {
				slog.Error("schedule goal", "error", err)
			}
		}
	}
}

func ensureGoalTaskContinuable(tx *sql.Tx, taskID string) error {
	var stopped bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM goal_tasks t JOIN goals g ON g.id=t.goal_id
WHERE t.task_id=? AND g.status='stopped')`, taskID).Scan(&stopped); err != nil {
		return err
	}
	if stopped {
		return ErrState
	}
	return nil
}
