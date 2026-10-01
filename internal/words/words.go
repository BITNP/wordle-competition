// Package words 从内置词表装载两张表并做判定。
//
// 词表由 web 包从 words/ 目录 embed（见 cmd/wordle），
// 答案池只在后端内存里；合法猜词表是两表的并集。
package words

import (
	"bufio"
	"fmt"
	"io/fs"
	"strings"
)

// WordLen 单词长度，写死 5，不走接口。
const WordLen = 5

type List struct {
	answers []string        // 可作答案，顺序即文件顺序（推导答案按下标取模）
	valid   map[string]bool // 合法猜词全集（含答案）
}

// Load 从 fsys 读入 answers.txt 与 valid.txt。两张表都必须是 5 字母小写词，否则启动失败。
func Load(fsys fs.FS) (*List, error) {
	answers, err := readList(fsys, "answers.txt")
	if err != nil {
		return nil, err
	}
	if len(answers) == 0 {
		return nil, fmt.Errorf("答案词表 answers.txt 是空的")
	}
	validWords, err := readList(fsys, "valid.txt")
	if err != nil {
		return nil, err
	}
	valid := make(map[string]bool, len(validWords)+len(answers))
	for _, w := range validWords {
		valid[w] = true
	}
	// 并集兜底：答案必须可猜，否则真答案会被判成 not_in_word_list。
	for _, w := range answers {
		valid[w] = true
	}
	return &List{answers: answers, valid: valid}, nil
}

func readList(fsys fs.FS, name string) ([]string, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, fmt.Errorf("读词表 %s: %w", name, err)
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		w := strings.TrimSpace(sc.Text())
		if w == "" || strings.HasPrefix(w, "#") {
			continue
		}
		if len(w) != WordLen {
			return nil, fmt.Errorf("词表 %s 里的 %q 不是 %d 个字符", name, w, WordLen)
		}
		for _, ch := range w {
			if ch < 'a' || ch > 'z' {
				return nil, fmt.Errorf("词表 %s 里的 %q 不是小写字母词", name, w)
			}
		}
		out = append(out, w)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读词表 %s: %w", name, err)
	}
	return out, nil
}

func (l *List) AnswerCount() int { return len(l.answers) }
func (l *List) ValidCount() int  { return len(l.valid) }

// AnswerAt 按下标取答案（下标由 game 的确定性推导给出，已取模）。
func (l *List) AnswerAt(i int) string {
	return l.answers[i]
}

// IsValid 判断是不是合法猜词。
func (l *List) IsValid(w string) bool { return l.valid[w] }

// Judge 返回定长 WordLen 位标记串：0 不在词中 / 1 在词中但位置错 / 2 位置正确。
//
// 两趟标记法：先标 2 并消耗字母，余下再标 1，重复字母按标准 Wordle 语义。
func Judge(answer, guess string) string {
	res := make([]byte, len(guess))
	for i := range res {
		res[i] = '0'
	}
	// 第一趟：位置正确
	var left [26]int
	for i := 0; i < len(answer) && i < len(guess); i++ {
		if answer[i] == guess[i] {
			res[i] = '2'
			continue
		}
		left[answer[i]-'a']++
	}
	// 第二趟：字母在答案里还有剩余就标 1
	for i := 0; i < len(guess); i++ {
		if res[i] == '2' {
			continue
		}
		c := guess[i] - 'a'
		if left[c] > 0 {
			left[c]--
			res[i] = '1'
		}
	}
	return string(res)
}
