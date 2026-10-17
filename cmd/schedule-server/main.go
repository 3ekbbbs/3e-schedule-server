// schedule-server 是一个自托管的日程同步服务端。
//
// 同一个 exe 有两种角色：
//
//	schedule-server                 管理器：拉起本地管理面板，并托管服务核心进程
//	schedule-server -run-server     服务核心：由管理器启动，通常不需要手动执行
//
// 这样做的好处是面板可以真正地启动 / 停止 / 重启服务，而不会把自己一起关掉。
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/3ekbbbs/schedule-server/internal/app"
	"github.com/3ekbbbs/schedule-server/internal/config"
	"github.com/3ekbbbs/schedule-server/internal/panel"
)

var (
	configFlag  = flag.String("config", "", "配置文件路径（默认：exe 同目录 config.json）")
	runServer   = flag.Bool("run-server", false, "以服务核心运行（由管理面板启动）")
	showVersion = flag.Bool("version", false, "打印版本号后退出")
	initOnly    = flag.Bool("init", false, "写入默认配置后退出")
	noBrowser   = flag.Bool("no-browser", false, "启动面板时不自动打开浏览器")
)

func main() {
	flag.Parse()

	exePath, err := os.Executable()
	if err != nil {
		fatal("无法定位程序位置", err)
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	baseDir := filepath.Dir(exePath)

	cfgPath := *configFlag
	if cfgPath == "" {
		cfgPath = filepath.Join(baseDir, "config.json")
	} else if !filepath.IsAbs(cfgPath) {
		cfgPath = filepath.Join(baseDir, cfgPath)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fatal("读取配置失败："+cfgPath, err)
	}
	if *showVersion {
		fmt.Println("schedule-server " + app.Version())
		return
	}
	if *initOnly {
		if err := config.Save(cfgPath, cfg); err != nil {
			fatal("写入配置失败", err)
		}
		fmt.Println("已写入默认配置：" + cfgPath)
		return
	}

	resolved := cfg.Resolve(baseDir)

	if *runServer {
		setupLogging(resolved.DataDir, "server.log")
		log.Printf("服务核心启动，配置：%s", cfgPath)
		if err := app.Run(resolved); err != nil {
			log.Printf("服务退出：%v", err)
			os.Exit(1)
		}
		return
	}

	// ---- 管理器模式 ----
	setupLogging(resolved.DataDir, "manager.log")
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		if err := config.Save(cfgPath, cfg); err != nil {
			fatal("写入默认配置失败", err)
		}
		log.Printf("已生成默认配置：%s", cfgPath)
	}

	panel.Version = app.Version()
	p := panel.New(panel.Options{
		Config:     cfg,
		ConfigPath: cfgPath,
		ExePath:    exePath,
		BaseDir:    baseDir,
	})
	if err := p.Start(); err != nil {
		fatal("管理面板启动失败", err)
	}
	log.Printf("管理面板已就绪：%s", p.URL())

	if cfg.PanelAutoOpen && !*noBrowser {
		openBrowser(p.URL())
	}
	p.Wait()
	log.Printf("管理器退出")
}

// setupLogging 把日志写进数据目录。GUI 子系统的 exe 没有有效 stderr，
// 所以只写文件。
func setupLogging(dataDir, name string) {
	dir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(f))
	log.SetFlags(log.LstdFlags)
}

// fatal 在 GUI 模式下没有控制台，必须把原因落到文件里，否则用户只看到
// 程序一闪而过。启动阶段的数据目录可能还没建好，所以退回临时目录。
func fatal(msg string, err error) {
	full := msg
	if err != nil {
		full += "：" + err.Error()
	}
	log.Print(full)
	dirs := []string{"."}
	if exe, e := os.Executable(); e == nil {
		dirs = append([]string{filepath.Dir(exe)}, dirs...)
	}
	line := time.Now().Format("2006-01-02 15:04:05") + " " + full + "\n"
	for _, d := range dirs {
		p := filepath.Join(d, "startup-error.log")
		if f, e := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); e == nil {
			_, _ = f.WriteString(line)
			_ = f.Close()
			break
		}
	}
	os.Exit(1)
}
