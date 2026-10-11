// Package game 持有全部过程状态：玩法、场次、每人进度、答案推导、判定、限速、榜单。
//
// 全在内存、单锁。重启即作废当前一场（中间态不落盘，已答对的成绩落库），操作员重新开场。
package game

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	"wordle/internal/store"
	"wordle/internal/words"
)

// State 一场比赛的状态。启动一律 waiting。
type State string

const (
	Waiting State = "waiting"
	Open    State = "open"
	Closed  State = "closed"
)

// Mode 玩法。
type Mode string

const (
	// ModeRound 轮次赛：一轮一题，解出即收工，按步数 → 用时排名。
	ModeRound Mode = "round"
	// ModeTimed 限时赛：限时内答对一题立刻换下一题继续答，按答对题数排名；时间到自动收场。
	ModeTimed Mode = "timed"
)

// minIntervalMs 两次被接受的猜测之间的最小间隔。写死常量，等价"1 次/秒"，
// 对真人极宽松，是唯一能挡住脚本穷举（12972 词空间）的手段。限时赛同样适用，
// 否则"限时内答对最多"会退化成"谁跑脚本跑得凶"。
const minIntervalMs = 1000

// 限时赛时长边界：太短来不及答完一题，太长会跨过整场活动。
const (
	MinDurationMs int64 = 30 * 1000
	MaxDurationMs int64 = 6 * 60 * 60 * 1000
)

// 拒绝原因，直接对应接口契约里的 error 值。
var (
	ErrRoundNotOpen    = errors.New("round_not_open")
	ErrExpired         = errors.New("expired")
	ErrAlreadySolved   = errors.New("already_solved")
	ErrBadLength       = errors.New("bad_length")
	ErrNotInWordList   = errors.New("not_in_word_list")
	ErrAlreadyGuessed  = errors.New("already_guessed")
	ErrTooManyRequests = errors.New("too_many_requests")
	ErrBadMode         = errors.New("bad_mode")
	ErrBadDuration     = errors.New("bad_duration")
)

type Game struct {
	mu     sync.Mutex
	secret []byte
	words  *words.List
	store  *store.Store
	logf   func(string, ...any)

	no       int64 // 自增场次号：1、2、3…，启动时从库里最大场次续号
	state    State
	mode     Mode
	openedAt int64 // 开场时刻（毫秒）
	endsAt   int64 // 限时赛截止时刻（毫秒）；轮次赛 0
	players  map[string]*progress
}

// progress 一个人的全部过程状态。
//
// 按"当前题"和"累计"两组看：限时赛答对一题后当前题那组整份重置、换下一题继续，
// 累计那组跨题保留；轮次赛不重置，一题到底。
type progress struct {
	name string

	answer  string          // 当前题的答案（只留内存）
	seen    map[string]bool // 当前题猜过的词 → already_guessed
	count   int             // 当前题被接受的猜测次数
	firstAt int64           // 当前题计时起点：首次被接受的猜测

	totalElapsed int64 // 累计净解题用时 = 各题用时之和，不含换题之间的空档

	lastAt int64 // 末次被接受的猜测（限速用，不随换题重置）

	lastSolvedAt int64 // 最近一次答对时刻（并列时兜底）

	solvedCount   int // 已答对题数
	totalAttempts int // 累计被接受的猜测次数（含每道答对的题）
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
		mode:    ModeRound, // 开场前也有个确定玩法，前端不必处理空值
		players: map[string]*progress{},
	}
	// 场次号是自增整数，重启后必须接着数而不是从 1 重来：否则新一场会和库里
	// 旧场次撞号，(round_no, sub) 这个主键会把旧成绩覆盖掉。
	// 只落已答对者，所以「没人答对的场次」不留痕迹、允许被复用（没有旧行可覆盖）。
	if st != nil {
		max, err := st.MaxRoundNo()
		if err != nil {
			logf("读历史最大场次失败，场次号从 0 开始: %v", err)
		}
		g.no = max
	}
	return g
}

// nowMs 服务端当前毫秒时刻。只有需要"到点自动收场"的读接口自己取时间，
// 判定接口仍由调用方注入 now，保持可测。
func nowMs() int64 { return time.Now().UnixMilli() }

// expireLocked 限时赛到点自动收场。状态转换放在读路径上做（懒收场）：
// 不需要后台定时器，最后一个读请求也会看到 closed。
func (g *Game) expireLocked(now int64) {
	if g.state == Open && g.mode == ModeTimed && g.endsAt > 0 && now >= g.endsAt {
		g.state = Closed
		g.logf("限时赛到点自动收场 round=%d", g.no)
	}
}

// Open 开新一场：场次号 +1（1、2、3…），上一场进度整份丢弃。
//
// mode 必须显式指定 ModeRound 或 ModeTimed；限时赛必须给 durationMs（MinDurationMs ~ MaxDurationMs）。
func (g *Game) Open(now int64, mode Mode, durationMs int64) (RoundStatus, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	switch mode {
	case ModeRound:
		durationMs = 0
	case ModeTimed:
		if durationMs < MinDurationMs || durationMs > MaxDurationMs {
			return RoundStatus{}, ErrBadDuration
		}
	default:
		return RoundStatus{}, ErrBadMode
	}

	g.no++
	g.state = Open
	g.mode = mode
	g.openedAt = now
	g.endsAt = 0
	if mode == ModeTimed {
		g.endsAt = now + durationMs
	}
	g.players = map[string]*progress{}
	return g.statusLocked(now), nil
}

// Close 收场。进度保留，榜单冻结成最终名次；关轮前到达的猜测仍算数。
func (g *Game) Close() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == Open {
		g.state = Closed
	}
	return g.state
}

// Outcome 一次被接受的猜测的结果，直接作为 POST /api/v1/guesses 的响应主体。
type Outcome struct {
	Result      string `json:"result"`
	Solved      bool   `json:"solved"`      // 这一次是否答对当前题
	SolvedCount int    `json:"solvedCount"` // 累计答对题数（轮次赛 0/1）
	Attempts    int    `json:"attempts"`    // 当前题的步数（本题答对时，即本题总步数）
	ElapsedMs   int64  `json:"elapsedMs"`   // 当前题用时，仅答对时有意义
}

// Guess 判一次猜测。now 取服务端毫秒时间戳，客户端时间一律不信。
//
// 顺序：到点收场 → 场次状态 → 已解出 → 限速 → 词校验。被拒的猜测不推导答案、
// 不计数、不计时（打错字不背锅）。
func (g *Game) Guess(sub, name, guess string, now int64) (Outcome, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.expireLocked(now)

	if g.state == Waiting {
		return Outcome{}, ErrRoundNotOpen
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
	if g.mode == ModeRound && p.solvedCount > 0 {
		return Outcome{}, ErrAlreadySolved
	}
	if g.state == Closed {
		return Outcome{}, ErrExpired
	}
	if p.lastAt != 0 && now-p.lastAt < minIntervalMs {
		return Outcome{}, ErrTooManyRequests
	}

	if len(guess) != words.WordLen {
		return Outcome{}, ErrBadLength
	}
	if !g.words.IsValid(guess) {
		return Outcome{}, ErrNotInWordList
	}
	if p.seen[guess] {
		return Outcome{}, ErrAlreadyGuessed
	}

	// 接受：这是唯一推进计时与计数的地方。
	p.lastAt = now
	p.count++
	p.totalAttempts++
	p.seen[guess] = true
	if p.firstAt == 0 {
		p.firstAt = now
	}
	if p.answer == "" {
		p.answer = g.deriveAnswer(sub, p.solvedCount+1)
	}

	result := words.Judge(p.answer, guess)
	out := Outcome{Result: result, Attempts: p.count, SolvedCount: p.solvedCount}
	if guess != p.answer {
		return out, nil
	}

	// 答对：先定格本题成绩并落库，再决定是收工（轮次赛）还是换题（限时赛）。
	elapsed := now - p.firstAt
	p.totalElapsed += elapsed
	p.lastSolvedAt = now
	p.solvedCount++
	out.Solved = true
	out.SolvedCount = p.solvedCount
	out.ElapsedMs = elapsed
	g.saveLocked(sub, p)

	if g.mode == ModeTimed {
		// 换下一题继续。seen 必须换新表，否则上一题猜过的词在下一题会被判
		// already_guessed；answer 清空，等下一次被接受的猜测再推导。
		p.answer = ""
		p.seen = map[string]bool{}
		p.count = 0
		p.firstAt = 0
	}
	return out, nil
}

// saveLocked 落一行成绩。限时赛每答对一题覆盖一次，万一进程崩了至少留下已答对的题数。
//
// 落库口径与玩法无关：solved_count 答对题数、attempts 累计步数、elapsed_ms 累计净解题
// 用时（各题用时之和，不含换题空档）。所以轮次赛与限时赛的成绩能落在同一张表里直接比较，
// 玩法差异只体现在名次规则上，不体现在列的含义上。
func (g *Game) saveLocked(sub string, p *progress) {
	if g.store == nil {
		return
	}
	err := g.store.SaveResult(store.Result{
		RoundNo:     g.no,
		Sub:         sub,
		DisplayName: p.name,
		Mode:        string(g.mode),
		SolvedCount: p.solvedCount,
		Attempts:    p.totalAttempts,
		ElapsedMs:   p.totalElapsed,
		SolvedAt:    p.lastSolvedAt,
	})
	if err != nil {
		// 落库失败不回滚成绩：榜单在内存里仍然正确，只记日志。
		g.logf("写成绩失败 round=%d sub=%s: %v", g.no, sub, err)
	}
}

// deriveAnswer 确定性推导某人第 seq 题的答案：
//
//	answer = 答案池[ HMAC-SHA256(secret, "assign|mode|roundNo|sub|seq") mod 答案池大小 ]
//
// 两种玩法使用同一格式，轮次赛 seq 恒为 1，限时赛 seq 随答对题数递增。
// 同一玩法、场次、选手与题号恒定，无需分配表；不同人允许碰撞（不要求互异）。
// 答案只留内存，绝不下发、绝不落盘、绝不进日志。
func (g *Game) deriveAnswer(sub string, seq int) string {
	mac := hmac.New(sha256.New, g.secret)
	payload := "assign|" + string(g.mode) + "|" + strconv.FormatInt(g.no, 10) + "|" + sub + "|" + strconv.Itoa(seq)
	mac.Write([]byte(payload))
	sum := mac.Sum(nil)
	return g.words.AnswerAt(int(binary.BigEndian.Uint64(sum[:8]) % uint64(g.words.AnswerCount())))
}

// RoundStatus 一场比赛的对外快照：选手页与管理面板共用同一份。
//
// 里面只有场次、玩法、时间与两个人数，没有 sub、没有答案，所以管理页投屏也不用担心。
type RoundStatus struct {
	State      State `json:"state"`
	RoundNo    int64 `json:"roundNo"`
	Mode       Mode  `json:"mode"`
	OpenedAt   int64 `json:"openedAt"`   // 毫秒时间戳，0 表示还没开过场
	DurationMs int64 `json:"durationMs"` // 限时赛总时长；轮次赛 0
	EndsAt     int64 `json:"endsAt"`     // 限时赛截止时刻；轮次赛 0
	ServerNow  int64 `json:"serverNow"`  // 服务端时刻：客户端据此校正本地时钟，倒计时才准
	Playing    int   `json:"playing"`    // 猜过至少一次、还没答对当前题的人数
	Solved     int   `json:"solved"`     // 至少答对一题的人数
}

// Status 当前一场的对外快照，供前端轮询。
func (g *Game) Status() RoundStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.statusLocked(nowMs())
}

func (g *Game) statusLocked(now int64) RoundStatus {
	g.expireLocked(now)
	s := RoundStatus{
		State:     g.state,
		RoundNo:   g.no,
		Mode:      g.mode,
		OpenedAt:  g.openedAt,
		EndsAt:    g.endsAt,
		ServerNow: now,
	}
	if g.mode == ModeTimed {
		s.DurationMs = g.endsAt - g.openedAt
	}
	for _, p := range g.players {
		if p.solvedCount > 0 {
			s.Solved++
		}
		if p.count > 0 && !(g.mode == ModeRound && p.solvedCount > 0) {
			s.Playing++ // 猜过至少一次、还没答对当前题
		}
	}
	return s
}

// Me 自己的进度，永不 null；字段固定，未猜过的数值均为 0，未上榜的 rank 为 0。
type Me struct {
	Solved        bool  `json:"solved"`        // 轮次赛：本轮已解出；限时赛：恒 false（可继续答）
	Rank          int   `json:"rank"`          // 未上榜为 0
	Attempts      int   `json:"attempts"`      // 当前题步数
	TotalAttempts int   `json:"totalAttempts"` // 累计步数
	SolvedCount   int   `json:"solvedCount"`   // 已答对题数
	ElapsedMs     int64 `json:"elapsedMs"`     // 轮次赛答对后的本题用时；其他情况为 0
}

// Entry 榜单一行。口径与玩法无关，名次规则才随玩法变。
type Entry struct {
	Rank        int    `json:"rank"`
	DisplayName string `json:"displayName"`
	SolvedCount int    `json:"solvedCount"` // 答对题数（轮次赛恒 1）
	Attempts    int    `json:"attempts"`    // 累计步数
	ElapsedMs   int64  `json:"elapsedMs"`   // 累计净解题用时（各题用时之和，不含换题空档）
}

// PlayerState 选手侧的一体快照：同一时刻的场次、榜单与个人进度。
type PlayerState struct {
	Round   RoundStatus `json:"round"`
	Entries []Entry     `json:"entries"`
	Me      Me          `json:"me"`
}

// Leaderboard 当前一场的快照、榜单（只有答对过至少一题的人）与自己的进度。
func (g *Game) Leaderboard(sub string) PlayerState {
	g.mu.Lock()
	defer g.mu.Unlock()
	round := g.statusLocked(nowMs())
	entries, me := g.boardLocked(sub)
	return PlayerState{Round: round, Entries: entries, Me: me}
}

// boardLocked 出榜。排序规则随玩法不同：
//
//	轮次赛：步数（少者强）→ 用时 → 答对时刻 → sub，全序；
//	限时赛：答对题数（多者强）→ 达到该题数的时刻（早者强）→ 累计步数 → sub。
//
// 限时赛的并列兜底用的是 lastSolvedAt（谁先达到这个题数），而不是净解题用时：
// 这是一场限时赛跑，先到者胜；净用时只是落库后用来比较解题速度的。
// 榜单在内存现排（≤200 条，微秒级），数据库不参与实时排名。
func (g *Game) boardLocked(sub string) ([]Entry, Me) {
	type row struct {
		sub string
		p   *progress
	}
	rows := make([]row, 0, len(g.players))
	for s, p := range g.players {
		if p.solvedCount > 0 {
			rows = append(rows, row{sub: s, p: p})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].p, rows[j].p
		if g.mode == ModeTimed {
			if a.solvedCount != b.solvedCount {
				return a.solvedCount > b.solvedCount
			}
			if a.lastSolvedAt != b.lastSolvedAt {
				return a.lastSolvedAt < b.lastSolvedAt
			}
			if a.totalAttempts != b.totalAttempts {
				return a.totalAttempts < b.totalAttempts
			}
			return rows[i].sub < rows[j].sub
		}
		if a.count != b.count {
			return a.count < b.count
		}
		if a.totalElapsed != b.totalElapsed {
			return a.totalElapsed < b.totalElapsed
		}
		if a.lastSolvedAt != b.lastSolvedAt {
			return a.lastSolvedAt < b.lastSolvedAt
		}
		return rows[i].sub < rows[j].sub
	})

	entries := make([]Entry, 0, len(rows))
	me := Me{}
	if p, ok := g.players[sub]; ok {
		me.Attempts = p.count
		me.Solved = g.mode == ModeRound && p.solvedCount > 0
		me.SolvedCount = p.solvedCount
		me.TotalAttempts = p.totalAttempts
		if me.Solved {
			me.ElapsedMs = p.totalElapsed
		}
	}
	for i, r := range rows {
		entries = append(entries, Entry{
			Rank:        i + 1,
			DisplayName: r.p.name,
			SolvedCount: r.p.solvedCount,
			Attempts:    r.p.totalAttempts,
			ElapsedMs:   r.p.totalElapsed,
		})
		if r.sub == sub {
			me.Rank = i + 1
		}
	}
	return entries, me
}

// AdminState 管理面板用：一场比赛的快照 + 实时榜单（投屏的就是这一页）。
type AdminState struct {
	Round RoundStatus `json:"round"`
	Board []Entry     `json:"board"`
}

func (g *Game) AdminState() AdminState {
	g.mu.Lock()
	defer g.mu.Unlock()
	round := g.statusLocked(nowMs())
	board, _ := g.boardLocked("")
	return AdminState{Round: round, Board: board}
}
