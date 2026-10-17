// Package panel 提供本地可视化管理面板，并负责拉起 / 停止服务核心进程。
//
// 设计要点：
//   - 面板与服务核心是两个进程。面板自己不能在“控制服务”时被一起停掉。
//   - 面板默认只监听 127.0.0.1，用服务端自己的管理员账号登录。
//   - 面板不直接读写业务数据库，所有业务操作都通过服务端 HTTP 接口完成，
//     避免绕过 seq / 权限 / 审计这些必须由服务核心维护的规则。
package panel

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/3ekbbbs/schedule-server/internal/config"
)

// Options 是面板运行所需的最小上下文。
type Options struct {
	Config     config.Config
	ConfigPath string
	ExePath    string
	BaseDir    string
}

// Panel 是管理面板实例。
type Panel struct {
	opt Options
	cfg config.Config // 已解析绝对路径的运行配置

	mu        sync.Mutex
	proc      *exec.Cmd
	procDone  chan struct{}
	startedAt time.Time
	stopping  bool
	lastErr   string
	restarts  int

	// relocating 挡住并发的"搬数据目录"：搬的过程中会停服务、改配置，
	// 两个请求同时进来会互相覆盖。
	relocating bool

	sessions map[string]*session

	httpSrv  *http.Server
	baseURL  string
	panelURL string

	consoleLog string
	serverLog  string
}

type session struct {
	Token   string
	Name    string
	IsAdmin bool
	Expires time.Time
}

// New 创建面板。不会启动任何监听。
func New(opt Options) *Panel {
	cfg := opt.Config.Resolve(opt.BaseDir)
	p := &Panel{
		opt:      opt,
		cfg:      cfg,
		sessions: map[string]*session{},
		baseURL:  proxyBase(cfg),
	}
	p.consoleLog = filepath.Join(cfg.DataDir, "logs", "console.log")
	p.serverLog = filepath.Join(cfg.DataDir, "logs", "server.log")
	return p
}

// Start 启动面板监听与服务核心进程。
func (p *Panel) Start() error {
	if err := os.MkdirAll(filepath.Join(p.cfg.DataDir, "logs"), 0o755); err != nil {
		return fmt.Errorf("创建日志目录失败: %w", err)
	}
	if err := os.MkdirAll(p.cfg.CertDir, 0o755); err != nil {
		return fmt.Errorf("创建证书目录失败: %w", err)
	}

	mux := http.NewServeMux()
	p.routes(mux)

	ln, err := net.Listen("tcp", p.cfg.PanelListen)
	if err != nil {
		return fmt.Errorf("管理面板监听 %s 失败: %w", p.cfg.PanelListen, err)
	}
	p.panelURL = panelURLFor(ln.Addr().String())
	p.httpSrv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		if err := p.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("管理面板退出：%v", err)
		}
	}()
	log.Printf("管理面板：http://%s", p.panelURL)

	// 服务进程启动失败不阻塞面板：用户需要在面板里看到失败原因并改配置。
	if err := p.StartServer(); err != nil {
		log.Printf("服务核心启动失败：%v", err)
	}
	return nil
}

// URL 返回面板地址，供入口打开浏览器。
func (p *Panel) URL() string { return "http://" + p.panelURL }

// Wait 阻塞到收到退出信号，然后停止服务并关闭面板。
func (p *Panel) Wait() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	signal.Stop(sigCh)
	log.Printf("收到退出信号，正在停止服务")
	_ = p.StopServer()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if p.httpSrv != nil {
		_ = p.httpSrv.Shutdown(ctx)
	}
}

// StartServer 启动服务核心子进程（幂等）。
func (p *Panel) StartServer() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startLocked()
}

func (p *Panel) startLocked() error {
	if p.proc != nil {
		return errors.New("服务已经在运行")
	}
	if err := os.MkdirAll(filepath.Join(p.cfg.DataDir, "logs"), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(p.consoleLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("打开控制台日志失败: %w", err)
	}
	cmd := exec.Command(p.opt.ExePath, "-run-server", "-config", p.opt.ConfigPath)
	cmd.Dir = p.opt.BaseDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setHidden(cmd)
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		p.lastErr = err.Error()
		return fmt.Errorf("启动服务进程失败: %w", err)
	}
	done := make(chan struct{})
	p.proc = cmd
	p.procDone = done
	p.startedAt = time.Now()
	p.stopping = false
	p.lastErr = ""
	log.Printf("服务进程已启动：pid=%d", cmd.Process.Pid)

	go func() {
		err := cmd.Wait()
		_ = logFile.Close()
		close(done)
		p.mu.Lock()
		if p.proc == cmd {
			p.proc = nil
		}
		stoppedByUser := p.stopping
		auto := p.opt.Config.AutoRestart
		p.mu.Unlock()
		if err != nil {
			log.Printf("服务进程退出：%v", err)
		} else {
			log.Printf("服务进程已退出")
		}
		if !stoppedByUser && auto {
			p.mu.Lock()
			p.restarts++
			n := p.restarts
			p.mu.Unlock()
			time.Sleep(time.Duration(minInt(n, 10)) * time.Second)
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.proc == nil && !p.stopping {
				if err := p.startLocked(); err != nil {
					log.Printf("自动重启失败：%v", err)
				}
			}
		}
	}()
	return nil
}

// StopServer 先请求优雅关闭，超时后强制结束。
func (p *Panel) StopServer() error {
	p.mu.Lock()
	cmd := p.proc
	done := p.procDone
	if cmd == nil || cmd.Process == nil {
		p.mu.Unlock()
		return nil
	}
	p.stopping = true
	p.mu.Unlock()

	// 优雅关闭：内部回环接口，只有拿到 0600 令牌文件才能调。
	if addr, token, ok := p.internalAddr(); ok {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/api/internal/shutdown", nil)
		req.Header.Set("X-Internal-Token", token)
		client := &http.Client{Timeout: 3 * time.Second}
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}
	select {
	case <-done:
		log.Printf("服务已优雅退出")
		return nil
	case <-time.After(8 * time.Second):
	}
	if err := cmd.Process.Kill(); err != nil {
		return fmt.Errorf("强制结束进程失败: %w", err)
	}
	<-done
	log.Printf("服务进程已被强制结束")
	return nil
}

// RestartServer 重启服务。
func (p *Panel) RestartServer() error {
	if err := p.StopServer(); err != nil {
		return err
	}
	return p.StartServer()
}

// internalAddr 读取服务进程写下的内部地址与令牌。
func (p *Panel) internalAddr() (addr, token string, ok bool) {
	b, err := os.ReadFile(filepath.Join(p.cfg.DataDir, "internal.addr"))
	if err != nil {
		return "", "", false
	}
	var v struct {
		Addr  string `json:"addr"`
		Token string `json:"token"`
	}
	if json.Unmarshal(b, &v) != nil || v.Addr == "" || v.Token == "" {
		return "", "", false
	}
	return v.Addr, v.Token, true
}

// health 查询服务核心的健康状态。
func (p *Panel) health() map[string]any {
	addr, token, ok := p.internalAddr()
	if !ok {
		return nil
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/api/internal/health", nil)
	req.Header.Set("X-Internal-Token", token)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out map[string]any
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil
	}
	return out
}

// running 返回进程状态快照。
func (p *Panel) running() (bool, int, int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.proc == nil || p.proc.Process == nil {
		return false, 0, 0
	}
	return true, p.proc.Process.Pid, int64(time.Since(p.startedAt).Seconds())
}

func (p *Panel) reloadConfig(c config.Config) {
	resolved := c.Resolve(p.opt.BaseDir)
	p.mu.Lock()
	p.opt.Config = c
	p.cfg = resolved
	p.baseURL = proxyBase(resolved)
	p.mu.Unlock()
}

func (p *Panel) snapshotConfig() config.Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opt.Config
}

func (p *Panel) base() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.baseURL
}

// sessionOf 从 Cookie 取会话。
func (p *Panel) sessionOf(r *http.Request) *session {
	c, err := r.Cookie("sp_session")
	if err != nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[c.Value]
	if !ok {
		return nil
	}
	if time.Now().After(s.Expires) {
		delete(p.sessions, c.Value)
		return nil
	}
	return s
}

// baseURLFor 根据监听配置推导面板访问地址（0.0.0.0 换成 127.0.0.1）。
func panelURLFor(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// proxyBase 推导服务端自身的 HTTP 基地址，供面板代理业务接口。
func proxyBase(c config.Config) string {
	scheme := "https"
	addr := c.Addr
	if addr == "" {
		scheme = "http"
		addr = c.HTTPAddr
	}
	if addr == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// 形如 ":8443" 或 "8443"
		port = strings.TrimPrefix(addr, ":")
		host = ""
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// proxyHTTP 返回访问本机服务端用的客户端。自签证书是默认情况，跳过校验。
func proxyHTTP() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     30 * time.Second,
		},
		Timeout: 0,
	}
}

// callServer 以指定令牌调用服务端接口，返回原始响应。
func (p *Panel) callServer(method, path, token string, body []byte) (*http.Response, error) {
	base := p.base()
	if base == "" {
		return nil, errors.New("服务端未配置监听地址")
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return proxyHTTP().Do(req)
}

// version 由 main 注入，避免 panel 反向依赖 app。
var Version = "dev"

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
