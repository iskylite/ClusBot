package work

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const benchmarkHintSchema = `
CREATE TABLE IF NOT EXISTS benchmark_event_hints (
 source TEXT NOT NULL,event_id TEXT NOT NULL,connection_key TEXT NOT NULL,binding_revision INTEGER NOT NULL,
 provider_job_id TEXT NOT NULL,received_at INTEGER NOT NULL, PRIMARY KEY(source,event_id),
 FOREIGN KEY(source,event_id) REFERENCES inbox_events(source,event_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS benchmark_hints_by_job ON benchmark_event_hints(connection_key,binding_revision,provider_job_id);
`

type BenchmarkEventReceiver struct {
	connection, secret string
	revision           int
}

func (p *HTTPBenchmarkProvider) EventReceiver(secret string) (*BenchmarkEventReceiver, error) {
	if p == nil || len(secret) < 32 || secret == p.credential {
		return nil, fmt.Errorf("%w：回执密钥必须独立且至少 32 字节", ErrBenchmarkBinding)
	}
	return &BenchmarkEventReceiver{p.connection, secret, p.revision}, nil
}

type benchmarkHint struct {
	EventID string `json:"event_id"`
	JobID   string `json:"job_id"`
}

// Receive persists only a wake-up hint. The payload cannot submit work, alter
// the reported result, or claim a Task ID. HTTPS/local termination is managed
// by the configured listener; timestamp and body are MAC'd by the bound source.
func (e *BenchmarkEventReceiver) Receive(ctx context.Context, s *Store, timestamp, signature string, body []byte) (bool, error) {
	if e == nil || len(body) > 8192 {
		return false, ErrAuthorizationDenied
	}
	stamp, err := strconv.ParseInt(timestamp, 10, 64)
	current := now()
	if err != nil || stamp < current-300000 || stamp > current+300000 {
		return false, ErrAuthorizationDenied
	}
	mac := hmac.New(sha256.New, []byte(e.secret))
	_, _ = mac.Write([]byte(timestamp + "\n"))
	_, _ = mac.Write(body)
	provided, err := hex.DecodeString(signature)
	if err != nil || !hmac.Equal(provided, mac.Sum(nil)) {
		return false, ErrAuthorizationDenied
	}
	if !json.Valid(body) {
		return false, ErrBenchmarkInput
	}
	var data benchmarkHint
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if dec.Decode(&data) != nil || !validBenchKey(data.EventID) || !validBenchKey(data.JobID) || data.JobID == "." || data.JobID == ".." || strings.ContainsAny(data.JobID, "?#") {
		return false, ErrBenchmarkInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	source := fmt.Sprintf("benchmark-provider:%s:%d", e.connection, e.revision)
	rec, err := receiveInbox(tx, InboxEvent{Source: source, Principal: source, Kind: "benchmark_job_hint", EventID: data.EventID,
		PayloadDigest: digest(toJSON(data)), ReceivedAt: current})
	if err != nil {
		return false, err
	}
	if rec.Quarantined {
		if err = tx.Commit(); err != nil {
			return false, err
		}
		return false, ErrConflict
	}
	if !rec.Duplicate {
		var full bool
		if err = tx.QueryRow(`SELECT count(*)>=1000 FROM benchmark_event_hints h JOIN inbox_events i
ON i.source=h.source AND i.event_id=h.event_id WHERE h.source=? AND i.state='received'`, source).Scan(&full); err != nil {
			return false, err
		}
		if full {
			return false, fmt.Errorf("%w：待关联的回执已达上限", ErrState)
		}
		if _, err = tx.Exec(`INSERT INTO benchmark_event_hints VALUES(?,?,?,?,?,?)`,
			source, data.EventID, e.connection, e.revision, data.JobID, current); err != nil {
			return false, err
		}
	}
	return rec.Duplicate, tx.Commit()
}

func (s *Store) applyBenchmarkHints(ctx context.Context, stamp int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT h.source,h.event_id,b.step_id FROM benchmark_event_hints h
JOIN inbox_events i ON i.source=h.source AND i.event_id=h.event_id
JOIN benchmark_job_identities j ON j.connection_key=h.connection_key AND j.binding_revision=h.binding_revision AND j.provider_job_id=h.provider_job_id
JOIN benchmark_cases b ON b.operation_id=j.operation_id
WHERE i.state='received' AND (b.phase<>'following' OR b.call_until<=?)
ORDER BY h.received_at,h.event_id LIMIT 50`, stamp)
	if err != nil {
		return err
	}
	var list [][3]string
	for rows.Next() {
		var v [3]string
		if err = rows.Scan(&v[0], &v[1], &v[2]); err != nil {
			rows.Close()
			return err
		}
		list = append(list, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range list {
		if _, err = tx.Exec("UPDATE benchmark_cases SET next_at=min(next_at,?) WHERE step_id=? AND phase='following' AND call_until<=?", stamp, v[2], stamp); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE inbox_events SET state='applied',applied_at=? WHERE source=? AND event_id=?", stamp, v[0], v[1]); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`UPDATE inbox_events SET state='quarantined',quarantine_reason='作业未关联，回执等待超过 24 小时'
WHERE state='received' AND kind='benchmark_job_hint' AND received_at<?`, stamp-86400000); err != nil {
		return err
	}
	return tx.Commit()
}
func migrateBenchmarkHints(tx *sql.Tx) error {
	if err := addColumnIfMissing(tx, "benchmark_cases", "call_until", "ALTER TABLE benchmark_cases ADD COLUMN call_until INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	_, err := tx.Exec(benchmarkHintSchema)
	return err
}
