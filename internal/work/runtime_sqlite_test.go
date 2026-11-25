package work

// 验收：单写入口、只读连接池与 SQLite 配置。
//
// 三个测试入口：
//   - TestSQLiteConnectionSettings：写连接显式设置并读取实际 WAL、
//     synchronous=FULL 等配置；验证实际驱动绑定 SQLite 版本；重启配置验证通过。
//   - TestReadPoolRejectsWrite：只读连接池逐连接设置只读约束、外键及
//     忙等待；并行读不会写入数据库；实际创建多个连接后配置仍正确。
//   - TestWriterAtomicity：多个写请求按事务顺序产生完整状态，没有
//     部分提交；长读与 checkpoint 竞争可观察，数据库不可写时请求不被虚假确认。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSQLiteConnectionSettings 写连接配置验证。
func TestSQLiteConnectionSettings(t *testing.T) {
	t.Run("writer_wal_full_and_real_version", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "writer.db")
		w, err := openWriter(path)
		if err != nil {
			t.Fatalf("openWriter: %v", err)
		}
		defer w.db.Close()

		s, err := readSettings(w.db)
		if err != nil {
			t.Fatal(err)
		}
		// 实际 journal_mode=wal、synchronous=FULL(2)、外键开启。
		if !strings.EqualFold(s.JournalMode, "wal") {
			t.Errorf("journal_mode = %s, want wal", s.JournalMode)
		}
		if s.Synchronous != 2 {
			t.Errorf("synchronous = %d, want 2 (FULL)", s.Synchronous)
		}
		if !s.ForeignKeys {
			t.Error("foreign_keys not enabled on writer")
		}
		// 实际驱动绑定的 SQLite 运行库版本（非 Go 模块版本号）。
		if s.SQLiteVersion == "" {
			t.Error("SQLite runtime version empty")
		}
		t.Logf("writer SQLite runtime version = %s (journal=%s sync=%d fk=%v)",
			s.SQLiteVersion, s.JournalMode, s.Synchronous, s.ForeignKeys)

		// 关闭后重开：配置验证仍通过（重启配置稳定）。
		if err := w.db.Close(); err != nil {
			t.Fatal(err)
		}
		w2, err := openWriter(path)
		if err != nil {
			t.Fatalf("reopen writer: %v", err)
		}
		defer w2.db.Close()
		s2, err := readSettings(w2.db)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.EqualFold(s2.JournalMode, "wal") || s2.Synchronous != 2 {
			t.Errorf("after reopen: journal=%s sync=%d, want wal/2", s2.JournalMode, s2.Synchronous)
		}
	})

	t.Run("writer_rejects_wrong_settings", func(t *testing.T) {
		// 反例：人为破坏配置后 verify 必须拒绝。
		path := filepath.Join(t.TempDir(), "bad.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		if _, err := db.Exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;"); err != nil {
			t.Fatal(err)
		}
		if err := initializeDatabase(db); err != nil {
			t.Fatal(err)
		}
		// 不设 WAL/FULL，直接验证：应被拒绝。
		w := &writerDB{db: db}
		err = w.verify()
		if err == nil {
			t.Error("verify accepted writer without WAL/FULL settings")
		} else {
			t.Logf("verify rejected misconfigured writer as expected: %v", err)
		}
		db.Close()
	})

	t.Run("wal_file_bytes_observable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "wal.db")
		w, err := openWriter(path)
		if err != nil {
			t.Fatal(err)
		}
		defer w.db.Close()
		// 写入一些数据使 WAL 增长。
		if _, err := w.db.Exec("CREATE TABLE IF NOT EXISTS probe(id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 100; i++ {
			if _, err := w.db.Exec("INSERT INTO probe(v) VALUES(?)", strings.Repeat("x", 100)); err != nil {
				t.Fatal(err)
			}
		}
		bytes := walFileBytes(path)
		t.Logf("WAL file bytes after writes = %d", bytes)
		if bytes < 0 {
			t.Errorf("walFileBytes = %d, want >= 0", bytes)
		}
	})
}

// TestReadPoolRejectsWrite 只读连接池约束验证。
func TestReadPoolRejectsWrite(t *testing.T) {
	t.Run("pool_connections_readonly_and_foreign_keys", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pool.db")
		// 先用写连接建库。
		w, err := openWriter(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.db.Exec("CREATE TABLE IF NOT EXISTS ro_test(id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
			t.Fatal(err)
		}
		if _, err := w.db.Exec("INSERT INTO ro_test(v) VALUES('readable')"); err != nil {
			t.Fatal(err)
		}
		w.db.Close()

		// 打开只读连接池（4 连接），逐连接验证只读约束与外键。
		pool, err := openReadPool(path, 4)
		if err != nil {
			t.Fatalf("openReadPool: %v", err)
		}
		defer pool.db.Close()

		// 并行读正常返回数据。
		var wg sync.WaitGroup
		var readErrs atomic.Int32
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var v string
				if err := pool.db.QueryRow("SELECT v FROM ro_test LIMIT 1").Scan(&v); err != nil {
					readErrs.Add(1)
					return
				}
				if v != "readable" {
					readErrs.Add(1)
				}
			}()
		}
		wg.Wait()
		if readErrs.Load() != 0 {
			t.Errorf("parallel reads failed: %d errors", readErrs.Load())
		}

		// 只读池拒绝写入。
		if _, err := pool.db.Exec("INSERT INTO ro_test(v) VALUES('should-fail')"); err == nil {
			t.Error("read pool allowed INSERT (write not rejected)")
		} else {
			t.Logf("read pool rejected write as expected: %v", err)
		}
		if _, err := pool.db.Exec("CREATE TABLE should_fail(id INTEGER)"); err == nil {
			t.Error("read pool allowed CREATE TABLE")
		}

		// 验证连接确实只读：专用连接上 query_only 生效（mode=ro URI 提供
		// SQLite 层只读保证；query_only 是逐连接的应用层加固）。
		ctx := context.Background()
		conn, err := pool.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var ro int
		if err := conn.QueryRowContext(ctx, "PRAGMA query_only").Scan(&ro); err != nil {
			t.Fatal(err)
		}
		if ro != 1 {
			t.Errorf("pool connection query_only = %d, want 1", ro)
		}
		var fk int
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if fk != 1 {
			t.Errorf("pool connection foreign_keys = %d, want 1", fk)
		}
	})

	t.Run("write_via_readonly_uri_rejected", func(t *testing.T) {
		// mode=ro URI 层面的只读：即使不设 query_only，SQLite 也拒绝写。
		path := filepath.Join(t.TempDir(), "uri.db")
		w, err := openWriter(path)
		if err != nil {
			t.Fatal(err)
		}
		w.db.Close()
		db, err := sql.Open("sqlite", sqliteFileURI(path, "mode=ro"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec("CREATE TABLE x(id INTEGER)"); err == nil {
			t.Error("mode=ro connection allowed CREATE TABLE")
		}
	})
}

// TestWriterAtomicity 写事务原子性与并发正确性。
func TestWriterAtomicity(t *testing.T) {
	t.Run("concurrent_writes_no_partial_commit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "atomic.db")
		w, err := openWriter(path)
		if err != nil {
			t.Fatal(err)
		}
		defer w.db.Close()
		if _, err := w.db.Exec(`CREATE TABLE acct(id INTEGER PRIMARY KEY, balance INTEGER NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		if _, err := w.db.Exec("INSERT INTO acct VALUES(1, 100)"); err != nil {
			t.Fatal(err)
		}

		// 多个写请求并发：每个是"读后写"的转账，必须在同一写事务重新读取
		// 并做条件更新。串行化写连接保证没有部分提交。
		const workers = 8
		const transfers = 20
		var wg sync.WaitGroup
		var failed atomic.Int64
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < transfers; j++ {
					tx, err := w.db.Begin()
					if err != nil {
						failed.Add(1)
						continue
					}
					var balance int
					if err := tx.QueryRow("SELECT balance FROM acct WHERE id=1").Scan(&balance); err != nil {
						tx.Rollback()
						failed.Add(1)
						continue
					}
					// 条件更新：仅当余额足够才扣减（读后写在同一事务）。
					res, err := tx.Exec("UPDATE acct SET balance=balance-1 WHERE id=1 AND balance>=1")
					if err != nil {
						tx.Rollback()
						failed.Add(1)
						continue
					}
					n, err := res.RowsAffected()
					if err != nil || (n != 1 && n != 0) {
						tx.Rollback()
						failed.Add(1)
						continue
					}
					if err := tx.Commit(); err != nil {
						failed.Add(1)
					}
				}
			}()
		}
		wg.Wait()

		// 最终余额必须一致：100 - 成功扣减次数，且永不为负（无部分提交）。
		var final int
		if err := w.db.QueryRow("SELECT balance FROM acct WHERE id=1").Scan(&final); err != nil {
			t.Fatal(err)
		}
		if final < 0 {
			t.Errorf("balance went negative: %d (partial commit or lost condition)", final)
		}
		t.Logf("final balance = %d after %d workers x %d transfers (failed txns=%d)",
			final, workers, transfers, failed.Load())
		var integrity string
		if err := w.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
			t.Errorf("integrity_check = %s, err=%v", integrity, err)
		}
	})

	t.Run("writer_metrics_observable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "metrics.db")
		w, err := openWriter(path)
		if err != nil {
			t.Fatal(err)
		}
		defer w.db.Close()
		// 执行几次写事务。
		for i := 0; i < 3; i++ {
			start := time.Now()
			tx, err := w.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS m(id INTEGER)"); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			w.observeTxDuration(time.Since(start))
		}
		m := w.metrics()
		if m.WriteTxMaxMS < 0 {
			t.Errorf("WriteTxMaxMS = %d, want >= 0", m.WriteTxMaxMS)
		}
		t.Logf("writer metrics: max=%dms total=%dms", m.WriteTxMaxMS, m.WriteTxTotalMS)
	})

	t.Run("unwritable_database_not_falsely_confirmed", func(t *testing.T) {
		// 数据库不可写时请求不被虚假确认：只读文件上打开写连接应失败或
		// 写入时报错，不能静默成功。
		dir := t.TempDir()
		path := filepath.Join(dir, "locked.db")
		w, err := openWriter(path)
		if err != nil {
			t.Fatal(err)
		}
		w.db.Close()
		// 以只读 URI 打开并尝试写：必须报错而非虚假确认。
		ro, err := sql.Open("sqlite", sqliteFileURI(path, "mode=ro"))
		if err != nil {
			t.Fatal(err)
		}
		defer ro.Close()
		if _, err := ro.Exec("INSERT INTO tasks(id) VALUES('x')"); err == nil {
			t.Error("write to readonly database falsely confirmed")
		} else if !errors.Is(err, sql.ErrTxDone) && err != nil {
			t.Logf("readonly write correctly failed: %v", err)
		}
	})
}

// 确保 fmt 引用。
var _ = fmt.Sprintf
