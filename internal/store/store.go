// Package store 只有一个结果表：谁、第几场、什么玩法、答对几题、几步、多久。
//
// 过程状态全在内存（internal/game），这里只做赛后凭证，不参与实时排名。
// 限时赛每答对一题覆盖写一次，所以中途崩了也能留下已经答对的题数。
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// 各列的含义与玩法无关，两种玩法的成绩落同一张表、可以直接比较；玩法差异只体现在
// 排序规则上（轮次赛看 attempts/elapsed_ms，限时赛看 solved_count），由 mode 标记。
const schema = `
CREATE TABLE IF NOT EXISTS results (
  round_no     INTEGER NOT NULL,
  sub          TEXT    NOT NULL,
  display_name TEXT    NOT NULL,
  mode         TEXT    NOT NULL DEFAULT 'round',  -- round 轮次赛 / timed 限时赛
  solved_count INTEGER NOT NULL DEFAULT 1,        -- 答对题数（轮次赛恒 1）
  attempts     INTEGER NOT NULL,   -- 累计被接受的猜测次数（含每道答对的题的最后那次正确提交）
  elapsed_ms   INTEGER NOT NULL,   -- 累计净解题用时 = 各题用时之和，不含换题之间的空档
  solved_at    INTEGER NOT NULL,   -- 最近一次答对的毫秒时间戳，并列时兜底
  PRIMARY KEY (round_no, sub)
);`

// 后加的列。SQLite 的 ALTER TABLE ADD COLUMN 不支持 IF NOT EXISTS，只能先查表结构再补。
var addedColumns = []struct{ name, ddl string }{
	{"mode", `ALTER TABLE results ADD COLUMN mode TEXT NOT NULL DEFAULT 'round'`},
	{"solved_count", `ALTER TABLE results ADD COLUMN solved_count INTEGER NOT NULL DEFAULT 1`},
}

type Store struct{ db *sql.DB }

// Result 一行成绩。列的含义与玩法无关，mode 只用来标记这场是怎么排名的。
type Result struct {
	RoundNo     int64
	Sub         string
	DisplayName string
	Mode        string // 空按轮次赛处理
	SolvedCount int    // 答对题数
	Attempts    int    // 累计步数
	ElapsedMs   int64  // 累计净解题用时
	SolvedAt    int64
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		fixPerm(dir, 0o755)
	}
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("打开数据库 %s: %w", path, err)
	}
	// 结果写入是低频事件，单写连接一次性规避 SQLITE_BUSY。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("建表: %w", err)
	}
	s := &Store{db: db}
	// 老库的 results 没有 mode/solved_count，启动时补上，不用手工迁移。
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	// 写完一次 schema 后 -wal/-shm 才存在，这时再兜一次权限。
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		fixPerm(f, 0o644)
	}
	return s, nil
}

// migrate 把 addedColumns 里缺的列补上。已经补过就是空操作，重复启动安全。
func (s *Store) migrate() error {
	rows, err := s.db.Query(`PRAGMA table_info(results)`)
	if err != nil {
		return fmt.Errorf("查结果表结构: %w", err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, typ        string
			dflt             any
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("读结果表结构: %w", err)
		}
		have[name] = true
	}
	err = rows.Err()
	rows.Close() // 单连接，必须先把 rows 收干净才能再 Exec
	if err != nil {
		return fmt.Errorf("读结果表结构: %w", err)
	}
	for _, col := range addedColumns {
		if have[col.name] {
			continue
		}
		if _, err := s.db.Exec(col.ddl); err != nil {
			return fmt.Errorf("补列 %s: %w", col.name, err)
		}
	}
	return nil
}

// SaveResult 落一行成绩；同一场同一人的记录由本次写入替换。
func (s *Store) SaveResult(r Result) error {
	if r.Mode == "" {
		r.Mode = "round"
	}
	if r.SolvedCount < 1 {
		r.SolvedCount = 1 // 只有答对才会写库，兜底避免存进 0
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO results
		   (round_no, sub, display_name, mode, solved_count, attempts, elapsed_ms, solved_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RoundNo, r.Sub, r.DisplayName, r.Mode, r.SolvedCount, r.Attempts, r.ElapsedMs, r.SolvedAt,
	)
	return err
}

// Close 优雅收尾：关连接时 WAL 会 checkpoint 回主库，之后只拷 wordle.db 就是完整数据。
func (s *Store) Close() error { return s.db.Close() }

// MaxRoundNo 库里出现过的最大场次号（没有任何记录时返回 0）。
// 启动时用它给自增场次号续号，避免新一场和旧场次撞号、覆盖掉旧成绩。
func (s *Store) MaxRoundNo() (int64, error) {
	var max int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(round_no), 0) FROM results`).Scan(&max)
	return max, err
}

// fixPerm 只在权限确实是 0000 时兜底放开。
//
// 有的文件系统（或父目录的默认 ACL）会让新建文件继承成 0000，这时只有 root
// 打不开也读不了：运维用 SQLite 工具查库、或者把 wordle.db 拷走都会报
// "unable to open database file"，而且看不出原因。权限只要不是 0 就不动，
// 不覆盖运维自己设过的值。
func fixPerm(path string, mode os.FileMode) {
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() == 0 {
		os.Chmod(path, mode)
	}
}
