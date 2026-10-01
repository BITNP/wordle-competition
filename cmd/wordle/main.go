// 在线 Wordle 竞赛后端：单进程单二进制 —— HTTP + 内存状态 + SQLite（只写结果）+ 内嵌前端。
//
// 用法：./wordle -config config.yaml
package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"wordle/internal/api"
	"wordle/internal/auth"
	"wordle/internal/config"
	"wordle/internal/game"
	"wordle/internal/store"
	"wordle/internal/words"
	"wordle/web"
)

func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[wordle] ")

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}

	// 词表：答案池 2315，合法猜词 12972（并集）。编译进二进制（web/words/），
	// /words/ 下发给选手的也是同一份文件。答案词不出现在任何日志里。
	wordFS, err := fs.Sub(web.FS, "words")
	if err != nil {
		log.Fatalf("启动失败: 取内置词表目录: %v", err)
	}
	lists, err := words.Load(wordFS)
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}
	log.Printf("词表就绪：可作答案 %d 个，合法猜词 %d 个", lists.AnswerCount(), lists.ValidCount())

	st, err := store.Open(cfg.DB.Path)
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}

	g := game.New([]byte(cfg.Auth.HMACSecret), lists, st, log.Printf)
	a := auth.New(cfg)
	srv := api.New(cfg, g, a, web.FS, log.Default())

	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// 猜词与轮询都是短请求，给足写超时即可，不做长连接。
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("监听 %s（前端已内嵌进二进制，数据库 %s）；启动后轮次为 waiting，等操作员开轮", cfg.Server.Addr, cfg.DB.Path)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务退出: %v", err)
		}
	}()

	// SIGTERM / SIGINT：优雅收尾，让 SQLite 把 WAL checkpoint 回主库，
	// 之后只拷 wordle.db 就是完整数据。
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Printf("收到退出信号，正在收尾")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("HTTP 关闭超时: %v", err)
	}
	if err := st.Close(); err != nil {
		log.Printf("关闭数据库失败: %v", err)
	}
	log.Printf("已退出")
}
