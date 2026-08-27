package main

import (
	"bufio"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const maxLogBytes = 10 << 20

const rotateRetryCooldown = 5 * time.Second

type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	f        *os.File
	size     int64
	maxSize  int64
	lastFail time.Time
}

func newRotatingWriter(path string, maxSize int64) (*rotatingWriter, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &rotatingWriter{path: path, f: f, size: info.Size(), maxSize: maxSize}, nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size > 0 && w.size+int64(len(p)) > w.maxSize {
		w.rotate()
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) rotate() {
	if !w.lastFail.IsZero() && time.Since(w.lastFail) < rotateRetryCooldown {
		return
	}
	_ = w.f.Close()
	_ = os.Remove(w.path + ".1")
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		w.reopen(w.path, w.size)
		w.lastFail = time.Now()
		return
	}
	nf, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		w.reopen(w.path+".1", -1)
		w.lastFail = time.Now()
		return
	}
	w.f = nf
	w.size = 0
	w.lastFail = time.Time{}
}

func (w *rotatingWriter) reopen(path string, sizeHint int64) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	w.f = f
	if sizeHint >= 0 {
		w.size = sizeHint
		return
	}
	if info, serr := f.Stat(); serr == nil {
		w.size = info.Size()
	}
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	return w.f.Close()
}

// extractCrontabLogEnv 直接从 crontab.txt 文件中扫描 WINCRON_LOG 环境变量设置
func extractCrontabLogEnv(crontabPath string) string {
	f, err := os.Open(crontabPath)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// 跳过空行和注释
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 遇到环境变量配置项 WINCRON_LOG=...
		if strings.HasPrefix(line, "WINCRON_LOG=") {
			val := strings.TrimPrefix(line, "WINCRON_LOG=")
			// 剥离可能存在的引号 (如 WINCRON_LOG="C:\log.log")
			val = strings.Trim(strings.TrimSpace(val), "\"'")
			return val
		}
	}
	return ""
}

func openLogger(crontabPath, path string, mirrorStdout bool) (*log.Logger, io.Closer, error) {
	// 1. 读取系统/进程级环境变量
	envLog := strings.TrimSpace(os.Getenv("WINCRON_LOG"))

	// 2. 若系统环境变量未设置，从 crontab.txt 文件提取
	if envLog == "" && crontabPath != "" {
		envLog = strings.TrimSpace(extractCrontabLogEnv(crontabPath))
	}

	// 3. 显式禁用日志 (off / none / null / NUL)
	if strings.EqualFold(envLog, "off") || strings.EqualFold(envLog, "none") || strings.EqualFold(envLog, "null") || strings.EqualFold(envLog, "nul") {
		var w io.Writer = io.Discard
		if mirrorStdout {
			w = os.Stdout
		}
		return log.New(w, "", log.LstdFlags), io.NopCloser(nil), nil
	}

	// 4. 若解析出自定义路径则覆盖默认路径
	if envLog != "" {
		path = envLog
	}

	rw, err := newRotatingWriter(path, maxLogBytes)
	if err != nil {
		return nil, nil, err
	}
	var w io.Writer = rw
	if mirrorStdout {
		w = io.MultiWriter(rw, os.Stdout)
	}
	return log.New(w, "", log.LstdFlags), rw, nil
}