package work

// P1-01 单写入口、只读连接池与 SQLite 配置。
//
// 本文件建立 SQLite 访问的基础设施：
//   - 写连接保留单连接/单写职责（所有业务写事务收敛到同一控制面写入者）；
//   - 只读连接池用于短只读查询，逐连接设置并验证只读约束、外键及忙等待；
//   - 显式设置并读取实际 WAL、synchronous=FULL 等配置，验证实际驱动绑定
//     的 SQLite 运行库版本（不用 Go 模块版本号代替运行库版本）；
//   - 为写等待、事务时长、WAL 大小和长读保留观测入口。
//
// 纪律：模型、网络、文件大复制不持有写事务；读后写判断必须在同一写事务
// 重新读取并做条件更新，不依赖只读池之前的缓存结论。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// SQLite 配置验证
// ---------------------------------------------------------------------------

// SQLiteSettings 是连接的关键配置实测值。
type SQLiteSettings struct {
	JournalMode   string // wal / delete / ...
	Synchronous   int    // 0=OFF 1=NORMAL 2=FULL 3=EXTRA
	ForeignKeys   bool   // PRAGMA foreign_keys
	BusyTimeout   int    // 毫秒
	ReadOnly      bool   // 连接是否只读
	SQLiteVersion string // 实际运行库版本（SELECT sqlite_version()）
	WALFileBytes  int64  // -wal 文件当前大小（观测入口）
}

// readSettings 读取一个连接的实际配置。
func readSettings(db *sql.DB) (SQLiteSettings, error) {
	var s SQLiteSettings
	if err := db.QueryRow("SELECT sqlite_version()").Scan(&s.SQLiteVersion); err != nil {
		return s, err
	}
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&s.JournalMode); err != nil {
		return s, err
	}
	if err := db.QueryRow("PRAGMA synchronous").Scan(&s.Synchronous); err != nil {
		return s, err
	}
	var fk int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		return s, err
	}
	s.ForeignKeys = fk == 1
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&s.BusyTimeout); err != nil {
		return s, err
	}
	// query_only 表示连接被设为只读（除写连接外，只读池应开启）。
	var ro int
	if err := db.QueryRow("PRAGMA query_only").Scan(&ro); err == nil {
		s.ReadOnly = ro == 1
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// 写连接（单写职责）
// ---------------------------------------------------------------------------

// writerDB 包装唯一的写连接，保留单写职责并记录观测数据。
type writerDB struct {
	db *sql.DB
	// 观测：写事务计数、累计时长、最大时长（P1-01.5）。
	txCount    atomic.Int64
	txTotalNS  atomic.Int64
	txMaxNS    atomic.Int64
	writeWaits atomic.Int64
}

// openWriter 打开并配置唯一写连接。
func openWriter(path string) (*writerDB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	return configureWriter(db)
}

func configureWriter(db *sql.DB) (*writerDB, error) {
	var err error
	// 单连接/单写职责：写连接串行化所有业务写事务。
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;"); err != nil {
		db.Close()
		return nil, err
	}
	if err = initializeDatabase(db); err != nil {
		db.Close()
		return nil, err
	}
	// 显式设置并读取实际 WAL 与 synchronous=FULL（P1-01.2）。
	if _, err = db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec("PRAGMA synchronous=FULL"); err != nil {
		db.Close()
		return nil, err
	}
	w := &writerDB{db: db}
	if err := w.verify(); err != nil {
		db.Close()
		return nil, err
	}
	return w, nil
}

// verify 校验写连接的实际配置符合要求。
func (w *writerDB) verify() error {
	s, err := readSettings(w.db)
	if err != nil {
		return err
	}
	if !strings.EqualFold(s.JournalMode, "wal") {
		return fmt.Errorf("写连接 journal_mode=%s，要求 wal", s.JournalMode)
	}
	if s.Synchronous != 2 {
		return fmt.Errorf("写连接 synchronous=%d，要求 2 (FULL)", s.Synchronous)
	}
	if !s.ForeignKeys {
		return errors.New("写连接 foreign_keys 未开启")
	}
	if s.SQLiteVersion == "" {
		return errors.New("无法读取实际 SQLite 运行库版本")
	}
	return nil
}

// BeginTx 开始一个写事务并记录观测。
func (w *writerDB) BeginTx(ctx context.Context) (*sql.Tx, error) {
	start := time.Now()
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		w.writeWaits.Add(1)
		return nil, err
	}
	w.txCount.Add(1)
	// 在事务提交/回滚时记录时长——通过包装不直接可行，改为调用方用
	// observeTxDuration；此处先计数，时长由写路径在提交时上报。
	_ = start
	return tx, nil
}

// observeTxDuration 记录一次写事务时长（由写路径在提交/回滚时调用）。
func (w *writerDB) observeTxDuration(d time.Duration) {
	ns := d.Nanoseconds()
	w.txTotalNS.Add(ns)
	for {
		max := w.txMaxNS.Load()
		if ns <= max || w.txMaxNS.CompareAndSwap(max, ns) {
			break
		}
	}
}

// ---------------------------------------------------------------------------
// 只读连接池
// ---------------------------------------------------------------------------

// readPool 是只读连接池：允许短只读查询并发，逐连接设置并验证只读约束、
// 外键及忙等待（P1-01.1）。只读查询不得写入数据库。
type readPool struct {
	db *sql.DB
	// 观测：长读计数（P1-01.5）。
	longReads atomic.Int64
	reads     atomic.Int64
}

// openReadPool 打开只读连接池。通过 DSN 参数在每个新物理连接建立时
// 自动应用只读约束、外键及忙等待（_query_only/_foreign_keys/_pragma），
// 并用 mode=ro 让 SQLite 在引擎层拒绝写入；这比事后 PRAGMA 更可靠，
// 因为 database/sql 会复用/新建连接而无需逐个手动配置。
func openReadPool(path string, maxConns int) (*readPool, error) {
	if maxConns < 1 {
		maxConns = 4
	}
	// DSN 层面：mode=ro（引擎只读）+ query_only（应用层加固）+
	// foreign_keys + busy_timeout。这些在每个新连接建立时自动生效。
	uri := sqliteFileURI(path, "mode=ro&_query_only=1&_foreign_keys=1&_busy_timeout=5000")
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxConns)
	p := &readPool{db: db}
	if err := p.verify(); err != nil {
		db.Close()
		return nil, err
	}
	return p, nil
}

// verify 验证只读池的约束真正生效（P1-01.1：不只是增大 SetMaxOpenConns）。
func (p *readPool) verify() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := p.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var ro, fk, bt int
	if err := conn.QueryRowContext(ctx, "PRAGMA query_only").Scan(&ro); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&bt); err != nil {
		return err
	}
	if ro != 1 {
		return fmt.Errorf("只读连接 query_only=%d，要求 1", ro)
	}
	if fk != 1 {
		return fmt.Errorf("只读连接 foreign_keys=%d，要求 1", fk)
	}
	if bt <= 0 {
		return fmt.Errorf("只读连接 busy_timeout=%d，要求 > 0", bt)
	}
	// 写入必须被拒绝（引擎层 mode=ro 保证）。
	if _, err := conn.ExecContext(ctx, "CREATE TABLE __ro_probe__(id INTEGER)"); err == nil {
		return errors.New("只读连接允许写入（CREATE TABLE 未被拒绝）")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 观测入口（P1-01.5）
// ---------------------------------------------------------------------------

// SQLiteMetrics 是写等待、事务时长、WAL 大小和长读的观测快照。
type SQLiteMetrics struct {
	WriteTxCount   int64
	WriteTxTotalMS int64
	WriteTxMaxMS   int64
	WriteWaits     int64
	ReadCount      int64
	LongReadCount  int64
	WALFileBytes   int64
}

// metrics 返回当前观测快照。
func (w *writerDB) metrics() SQLiteMetrics {
	return SQLiteMetrics{
		WriteTxCount:   w.txCount.Load(),
		WriteTxTotalMS: w.txTotalNS.Load() / int64(time.Millisecond),
		WriteTxMaxMS:   w.txMaxNS.Load() / int64(time.Millisecond),
		WriteWaits:     w.writeWaits.Load(),
	}
}

func (p *readPool) metrics() (reads, longReads int64) {
	return p.reads.Load(), p.longReads.Load()
}

// walFileBytes 读取 -wal 文件当前大小（观测入口）。
func walFileBytes(dbPath string) int64 {
	info, err := os.Stat(dbPath + "-wal")
	if err != nil {
		return 0
	}
	return info.Size()
}
