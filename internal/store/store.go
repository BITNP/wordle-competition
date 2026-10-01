// Package store 只有一个结果表：谁、第几轮、几步、多久。
//
// 过程状态全在内存（internal/game），这里只做赛后凭证，不参与实时排名。
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS results (
  round_no     INTEGER NOT NULL,
  sub          TEXT    NOT NULL,
  display_name TEXT    NOT NULL,
  attempts     INTEGER NOT NULL,   -- 几次猜出（含最后那次正确提交）
  elapsed_ms   INTEGER NOT NULL,   -- 首次被接受的猜测 → 正确提交
  solved_at    INTEGER NOT NULL,   -- 毫秒时间戳，并列时兜底
  PRIMARY KEY (round_no, sub)
);`

type Store struct{ db *sql.DB }

// Result 一行成绩。
type Result struct {
	RoundNo     int64
	Sub         string
	DisplayName string
	Attempts    int
	ElapsedMs   int64
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
	// 写完一次 schema 后 -wal/-shm 才存在，这时再兜一次权限。
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		fixPerm(f, 0o644)
	}
	return &Store{db: db}, nil
}

// SaveResult 落一行成绩；同一轮同一人的记录由本次写入替换。
func (s *Store) SaveResult(r Result) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO results
		   (round_no, sub, display_name, attempts, elapsed_ms, solved_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.RoundNo, r.Sub, r.DisplayName, r.Attempts, r.ElapsedMs, r.SolvedAt,
	)
	return err
}

// Close 优雅收尾：关连接时 WAL 会 checkpoint 回主库，之后只拷 wordle.db 就是完整数据。
func (s *Store) Close() error { return s.db.Close() }

// MaxRoundNo 库里出现过的最大轮次号（没有任何记录时返回 0）。
// 启动时用它给自增轮次号续号，避免新一轮和旧轮次撞号、覆盖掉旧成绩。
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
