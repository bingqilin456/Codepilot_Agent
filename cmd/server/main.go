package main

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if err := godotenv.Load(".env"); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		logger.Error("加载 .env 失败，请检查文件权限与格式")
		os.Exit(1)
	}

	addr := os.Getenv("CODEPILOT_HTTP_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	router := gin.Default()

	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	logger.Info("准备启动 HTTP 服务",
		"addr", server.Addr,
		"read_header_timeout", server.ReadHeaderTimeout,
		"read_timeout", server.ReadTimeout,
		"write_timeout", server.WriteTimeout,
		"idle_timeout", server.IdleTimeout,
	)

	if err := server.ListenAndServe(); err != nil &&
		!errors.Is(err, http.ErrServerClosed) {
		logger.Error("HTTP 服务启动或运行失败",
			"addr", server.Addr,
			"err", err,
		)
		os.Exit(1)
	}
}
