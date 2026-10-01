// Package game 持有全部过程状态：轮次、每人进度、答案推导、判定、限速、榜单。
//
// 全在内存、单锁。重启即作废本轮（中间态不落盘），操作员重新开轮。
package game

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"strconv"
	"sync"

	"wordle/internal/store"
	"wordle/internal/words"
)

// State 轮次状态。启动一律 waiting。
type State string

const (
	Waiting State = "waiting"
	Open    State = "open"
	Closed  State = "closed"
)

// minIntervalMs 两次被接受的猜测之间的最小间隔。写死常量，等价"1 次/秒"，
// 对真人极宽松，是唯一能挡住脚本穷举（12972 词空间）的手段。
const minIntervalMs = 1000

// 拒绝原因，直接对应接口契约里的 error 值。
var (
	ErrRoundNotOpen    = errors.New("round_not_open")
	ErrExpired         = errors.New("expired")
	ErrAlreadySolved   = errors.New("already_solved")
	ErrBadLength       = errors.New("bad_length")
	ErrNotInWordList   = errors.New("not_in_word_list")
	ErrAlreadyGuessed  = errors.New("already_guessed")
	ErrTooManyRequests = errors.New("too_many_requests")
)

type Game struct {
	mu     sync.Mutex
	secret []byte
	words  *words.List
	store  *store.Store
	logf   func(string, ...any)

	no       int64 // 自增轮次号：1、2、3…，启动时从库里最大轮次续号
	state    State
	openedAt int64 // 开轮时刻（毫秒，仅管理面板显示用）
	players  map[string]*progress
}

type progress struct {
	name     string
	answer   string
	seen     map[string]bool // 本轮猜过的词 → already_guessed
	count    int             // 被接受的猜测次数
	firstAt  int64           // 计时起点：首次被接受的猜测
	lastAt   int64           // 末次被接受的猜测（限速用）
	solved   bool
	solvedAt int64
	elapsed  int64
}

func New(secret []byte, w *words.List, st *store.Store, logf func(string, ...any)) *Game {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	g := &Game{
		secret:  secret,
		words:   w,
		store:   st,
		logf:    logf,
		state:   Waiting,
		players: map[string]*progress{},
	}
	// 轮次号是自增整数，重启后必须接着数而不是从 1 重来：否则新一轮会和库里
	// 旧轮次撞号，(round_no, sub) 这个主键会把旧成绩覆盖掉。
	// 只解出者才落库，所以「没人解出的轮次」不留痕迹、允许被复用（没有旧行可覆盖）。
	if st != nil {
		max, err := st.MaxRoundNo()
		if err != nil {
			logf("读历史最大轮次失败，轮次号从 0 开始: %v", err)
		}
		g.no = max
	}
	return g
}

// Round 当前轮次状态，供前端轮询。
func (g *Game) Round() (State, int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state, g.no
}

// Open 开新一轮：轮次号 +1（1、2、3…），上一轮进度整份丢弃。
func (g *Game) Open(now int64) (int64, State) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.no++
	g.state = Open
	g.openedAt = now
	g.players = map[string]*progress{}
	return g.no, g.state
}

// Close 关轮。进度保留，榜单冻结成最终名次；关轮前到达的猜测仍算数。
func (g *Game) Close() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == Open {
		g.state = Closed
	}
	return g.state
}

// Guess 判一次猜测。now 取服务端毫秒时间戳，客户端时间一律不信。
//
// 顺序：轮次状态 → 已解出 → 限速 → 词校验。被拒的猜测不推导答案、
// 不计数、不计时（打错字不背锅）。
func (g *Game) Guess(sub, name, guess string, now int64) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.state == Waiting {
		return "", ErrRoundNotOpen
	}
	p, ok := g.players[sub]
	if !ok {
		// 先占位，答案等第一次被接受的猜测再推导（被拒的猜测不推导）。
		p = &progress{name: name, seen: map[string]bool{}}
		g.players[sub] = p
	}
	if name != "" {
		p.name = name
	}
	if p.solved {
		return "", ErrAlreadySolved
	}
	if g.state == Closed {
		return "", ErrExpired
	}
	if p.lastAt != 0 && now-p.lastAt < minIntervalMs {
		return "", ErrTooManyRequests
	}

	if len(guess) != words.WordLen {
		return "", ErrBadLength
	}
	if !g.words.IsValid(guess) {
		return "", ErrNotInWordList
	}
	if p.seen[guess] {
		return "", ErrAlreadyGuessed
	}

	// 接受：这是唯一推进计时与计数的地方。
	p.lastAt = now
	p.count++
	p.seen[guess] = true
	if p.firstAt == 0 {
		p.firstAt = now
	}
	if p.answer == "" {
		p.answer = g.deriveAnswer(sub)
	}

	result := words.Judge(p.answer, guess)
	if guess == p.answer {
		p.solved = true
		p.solvedAt = now
		p.elapsed = now - p.firstAt
		if g.store != nil {
			err := g.store.SaveResult(store.Result{
				RoundNo:     g.no,
				Sub:         sub,
				DisplayName: p.name,
				Attempts:    p.count,
				ElapsedMs:   p.elapsed,
				SolvedAt:    p.solvedAt,
			})
			if err != nil {
				// 落库失败不回滚成绩：本轮榜单在内存里仍然正确，只记日志。
				g.logf("写成绩失败 round=%d sub=%s: %v", g.no, sub, err)
			}
		}
	}
	return result, nil
}

// deriveAnswer 确定性推导每人每轮的答案：
//
//	answer = 答案池[ HMAC-SHA256(secret, "assign|round|sub") mod 2315 ]
//
// 同一轮同一人恒定、重启后一致、无需分配表；不同人允许碰撞（不要求互异）。
// 答案只留内存，绝不下发、绝不落盘、绝不进日志。
func (g *Game) deriveAnswer(sub string) string {
	mac := hmac.New(sha256.New, g.secret)
	payload := append([]byte("assign|"), strconv.FormatInt(g.no, 10)...)
	payload = append(payload, '|')
	payload = append(payload, sub...)
	mac.Write(payload)
	sum := mac.Sum(nil)
	return g.words.AnswerAt(int(binary.BigEndian.Uint64(sum[:8]) % uint64(g.words.AnswerCount())))
}

// Me 自己的进度，永不 null（未猜过就是 0 步、未解出）。
type Me struct {
	Solved    bool  `json:"solved"`
	Rank      int   `json:"rank,omitempty"`
	Attempts  int   `json:"attempts"`
	ElapsedMs int64 `json:"elapsedMs,omitempty"`
}

// Entry 榜单一行。
type Entry struct {
	Rank        int    `json:"rank"`
	DisplayName string `json:"displayName"`
	Attempts    int    `json:"attempts"`
	ElapsedMs   int64  `json:"elapsedMs"`
}

// Leaderboard 当前轮仅解出者，按 attempts（步数）→ elapsedMs → solvedAt → sub 排成全序。
// 步数优先：少一步就是更强，用时只在步数相同时才比较。
// 榜单在内存现排（≤200 条，微秒级），数据库不参与实时排名。
func (g *Game) Leaderboard(sub string) ([]Entry, Me, int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	entries, me := g.boardLocked(sub)
	return entries, me, g.no
}

func (g *Game) boardLocked(sub string) ([]Entry, Me) {
	type row struct {
		sub, name string
		p         *progress
	}
	rows := make([]row, 0, len(g.players))
	for s, p := range g.players {
		if p.solved {
			rows = append(rows, row{sub: s, name: p.name, p: p})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].p, rows[j].p
		if a.count != b.count {
			return a.count < b.count
		}
		if a.elapsed != b.elapsed {
			return a.elapsed < b.elapsed
		}
		if a.solvedAt != b.solvedAt {
			return a.solvedAt < b.solvedAt
		}
		return rows[i].sub < rows[j].sub
	})

	entries := make([]Entry, 0, len(rows))
	me := Me{}
	if p, ok := g.players[sub]; ok {
		me.Attempts = p.count
		me.Solved = p.solved
		me.ElapsedMs = p.elapsed
	}
	for i, r := range rows {
		entries = append(entries, Entry{
			Rank:        i + 1,
			DisplayName: r.name,
			Attempts:    r.p.count,
			ElapsedMs:   r.p.elapsed,
		})
		if r.sub == sub {
			me.Rank = i + 1
		}
	}
	return entries, me
}

// RoundInfo 管理面板看到的轮次信息。
type RoundInfo struct {
	No       int64 `json:"no"`
	State    State `json:"state"`
	OpenedAt int64 `json:"openedAt"` // 毫秒时间戳，0 表示还没开过轮
	Playing  int   `json:"playing"`  // 本轮猜过至少一次、还没解出的人数
	Solved   int   `json:"solved"`   // 本轮已解出的人数
}

type AdminState struct {
	Round RoundInfo `json:"round"`
	Board []Entry   `json:"board"`
}

// AdminState 管理面板用：轮次概览 + 实时榜单。
func (g *Game) AdminState() AdminState {
	g.mu.Lock()
	defer g.mu.Unlock()
	board, _ := g.boardLocked("")
	info := RoundInfo{No: g.no, State: g.state, OpenedAt: g.openedAt}
	for _, p := range g.players {
		if p.solved {
			info.Solved++
			continue
		}
		if p.count > 0 {
			info.Playing++ // 猜过至少一次、还没解出
		}
	}
	return AdminState{Round: info, Board: board}
}
