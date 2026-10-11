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

// 各列的含义与玩法无关，两种玩法的成绩落同一张表；玩法差异体现在
// 排序规则上（轮次赛看 attempts/elapsed_ms，限时赛看 solved_count），由 mode 标记。
const schema = `
CREATE TABLE IF NOT EXISTS results (
  round_no     INTEGER NOT NULL,
  sub          TEXT    NOT NULL,
  display_name TEXT    NOT NULL,
  mode         TEXT    NOT NULL,   -- round 轮次赛 / timed 限时赛
  solved_count INTEGER NOT NULL,   -- 答对题数（轮次赛恒 1）
  attempts     INTEGER NOT NULL,   -- 累计被接受的猜测次数（含每道答对的题的最后那次正确提交）
  elapsed_ms   INTEGER NOT NULL,   -- 累计净解题用时 = 各题用时之和，不含换题之间的空档
  solved_at    INTEGER NOT NULL,   -- 最近一次答对的毫秒时间戳，并列时兜底
  PRIMARY KEY (round_no, sub)
);`

type Store struct{ db *sql.DB }

// Result 一行成绩。列的含义与玩法无关，mode 只用来标记这场是怎么排名的。
type Result struct {
	RoundNo     int64
	Sub         string
	DisplayName string
	Mode        string // 玩法由游戏层规范化
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
	return &Store{db: db}, nil
}

// SaveResult 落一行成绩；同一场同一人的记录由本次写入替换。
func (s *Store) SaveResult(r Result) error {
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
