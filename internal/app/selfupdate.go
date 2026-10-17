// 主服务端自己的热更新：把"服务器上那个 .new.exe"真正换上去并重启。
//
// 2026-09-21 从 web/selfupdate.go 移植过来。
// 来由：网页端早就有这条通道，**主服务端一直什么都没有** —— 换一次 exe 就得
// 上一次服务器敲命令，而"能上服务器"这件事不是每次都成立。
// 结果是客户端发了版、服务端还停在两个版本前，新接口全是 404
// （用户原话："服务端程序上是看到了，但接口全都没连上"）。
//
// 下面这段是原来网页端的说明，一字未改 —— 四条坑是同一个二进制替换问题，
// 与"是网页端还是主服务端"无关。
//
// 为什么需要它：`/api/deploy/server` 早就支持连服务本体一起传了，但它只做到
// **暂存**（`schedule-server.new.exe`）—— 之后要有人上服务器敲
// `taskkill` → `move` → `schtasks /Run`。而"能上服务器敲命令"这件事本身
// 不是每次都成立（远程执行通道会整轮不可用），于是「把热更新做好」这件事
// 就卡在最后一步上，一次卡一个版本。
//
// 这里把它补完：收到新 exe **当场安排**一次替换 + 重启，不再需要人插手。
//
// 三个必须绕开的坑：
//
//  1. **运行中的 exe 在 Windows 上不能被覆盖，也不能被删**。所以不能在本进程里
//     "停下自己 → 改名 → 起来" —— 停下的那一刻就没有代码可执行了。
//     做法是把替换动作交给一个**脱离本进程**的 cmd 脚本，本进程在它起来之后
//     自己退出，让出文件句柄。
//
//  2. **重启要能"回到原来的样子"** —— 端口、证书目录、上游地址这些都在启动
//     参数里。计划任务 `schedule-server` 拉起来的是不带参数的裸 exe，靠的是
//     exe 旁边的默认值；但为了不在"参数到底是什么"这件事上赌一把，脚本会把
//     **本次进程的实际命令行**原样传回去（`os.Args` 里除 exe 自己以外的部分）。
//
//  3. **连不上/起不来时要能回滚**。所以换之前先备份成 `.before-<版本>.exe`，
//     脚本起完新进程等一会儿探一次 `/api/health`，探不到就把备份换回来再起一次。
//     Windows 上这一步不能靠"等进程退出"来判，只能靠探测 HTTP 端口。
//
// 只认 `/api/deploy/server` 传来的那个固定文件名 —— 这个接口本身有令牌保护，
// 而这里再收一道：路径永远是 exeDir 下的 `schedule-server.new.exe`。
package app

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// stagedExeName 是部署接口给新 exe 用的暂存名。换的时候也按这个名字找。
const stagedExeName = "schedule-server.new.exe"

// selfReplaceCmd 是替换脚本的文件名，固定（每次覆盖）。
const selfReplaceCmd = "self-update.cmd"

// selfUpdateTask 是那个一次性计划任务的名字。它唯一的用途是让脚本**脱离本进程树**
// 跑起来（见 runDetached），跑完脚本自己会把它删掉。
const selfUpdateTask = "schedule-server-selfupdate"

// stagedExe 返回暂存的新 exe 路径；没有就返回空串。
func stagedExe() string {
	p := filepath.Join(exeDir(), stagedExeName)
	if fileExists(p) {
		return p
	}
	return ""
}

// hasStaged 给 /api/health 用：让它能直接回答"有没有新版本在等着重启"。
func hasStaged() bool { return stagedExe() != "" }

// scheduleSelfReplace 安排"换成新 exe 并重启"，然后让调用方尽快退出进程。
//
// 返回 nil 只表示"脚本已经甩出去了"，不表示新版本已经起来 —— 那是脚本自己的事，
// 结果只能靠重启之后来看 `/api/health` 的版本号。这一点在接口响应里说清楚。
func scheduleSelfReplace(reason string) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("自替换目前只在 Windows 上实现（服务器是 Windows Server）")
	}
	staged := stagedExe()
	if staged == "" {
		return fmt.Errorf("没有找到 %s", stagedExeName)
	}
	bakExt := ".before-" + version

	body := buildSelfReplaceScript(bakExt)
	cmdPath := filepath.Join(exeDir(), selfReplaceCmd)
	if err := os.WriteFile(cmdPath, []byte(body), 0o755); err != nil {
		return fmt.Errorf("无法写入替换脚本：%v", err)
	}

	// ⚠️ 起替换脚本这一步**不能用 exec.Command + DETACHED_PROCESS**。
	//
	// 实测过：那样起出来的 cmd 仍然是本进程的后代，本进程 `os.Exit(0)` 的瞬间
	// 它就被一起收走了 —— 现象是"脚本生成了、进程死了、文件没换、备份也没有"
	// （脚本卡在等待循环里就没了）。文件系统上留下的证据很好认：
	// `self-update.cmd` 还在、`.new.exe` 还在、`.before-*.exe` 从来没出现过。
	//
	// 可靠的做法是**让任务计划程序去起它**：那是一个独立的服务进程，
	// 脚本挂到它底下，与本进程的死活彻底无关。项目里早就有先例
	// （第一次把新版 exe 送上去时用的就是这招）。
	//
	// `/f` 覆盖同名任务，`/sc once /st` 给一个很快就到的时间 —— 任务本身
	// 跑完就自删，不留垃圾。任务名固定，下次部署会覆盖掉上一次的。
	if err := runDetached(cmdPath); err != nil {
		return err
	}
	log.Printf("[self-update] 已安排替换并重启（%s）：%s → schedule-server.exe（备份 %s）",
		reason, stagedExeName, "schedule-server"+bakExt+".exe")

	// 让响应先飞回去，再退出。调用方（部署脚本）拿到 200 + 一句
	// "正在重启"就够，不必等新进程起来。
	go func() {
		time.Sleep(1200 * time.Millisecond)
		log.Printf("[self-update] 进程退出，把 exe 文件句柄让给替换脚本")
		os.Exit(0)
	}()
	return nil
}

// selfExePath 是当前进程的可执行文件路径。
func selfExePath() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return p
}

// runDetached 让**任务计划程序**去跑那个替换脚本，而不是自己 fork 一个子进程。
//
// 为什么非得这么绕：本进程随后就要 `os.Exit(0)` 把 exe 文件句柄让出去，
// 而它 fork 出来的任何子进程都跟着它一起没了（DETACHED_PROCESS 也不行，
// 实测过 —— 脚本卡在第一步，连备份文件都没生成）。任务计划程序是一个独立的
// 系统服务，它拉起来的进程与本进程的生死无关 —— 这正是我们要的。
//
// ⚠️ **必须显式写 `/ru SYSTEM`**。`schtasks /create` 默认拿"当前登录用户"当
// 运行账户，而我们这个进程可能是被计划任务 / 远程执行通道拉起来的，那种上下文里
// 查不到一个可用的用户名，于是报
//
//	ERROR: No mapping between account names and security IDs was done.
//
// 而且它是**只在 stderr 里的失败**：不加 /ru 时任务根本建不出来，代码却以为
// "建好了"，接着 /run 又失败，最后退回那条不可靠的 fork 路径。实测两台机器上
// 都是这个现象。SYSTEM 和 `schedule-server` 任务用的是同一个账户，口径一致。
//
// 用 `/create ... /f` + `/run`，而不是 `/create` 之后等它到点自己跑：
// `/st` 只精确到分钟，等一分钟太久；`/run` 立刻触发。
// 任务名固定，跑完脚本会自删，下次部署再覆盖。
func runDetached(cmdPath string) error {
	const taskName = selfUpdateTask

	// ⚠️ `/tr` 的值是**一个普通参数**，不要再往里塞 `\"` 那一层。
	//
	// 踩过的坑：写成 `"\"\\\"" + cmdPath + "\\\"\""`（那是给 PowerShell 传参时
	// 才需要的双层转义）会生成
	//
	//     Task To Run: "\" C:\...\self-update.cmd\""
	//
	// 引号错位、中间还多一个空格 —— 任务建得出来、`/run` 也"成功"，但一跑就
	// `Last Result: -2147024891`（0x80070005 拒绝访问），脚本根本没启动。
	//
	// exec.Command 不做 shell 解析，参数原样传给进程；schtasks 自己要的是
	// "路径含空格时用引号包起来"，就这一层。
	tr := "\"" + cmdPath + "\""

	create := exec.Command("schtasks.exe",
		"/create",
		"/tn", taskName,
		"/tr", tr,
		"/sc", "once",
		"/st", "00:00",
		"/ru", "SYSTEM",
		"/f",
	)
	create.Dir = exeDir()
	if out, err := create.CombinedOutput(); err != nil {
		// 建不了任务就直接退回"自己 fork"——虽然不可靠，但总比什么都不做好。
		// 真实原因写进日志，别让它变成一个静默的失败。
		log.Printf("[self-update] 建计划任务失败（%v）：%s；退回直接启动", err, strings.TrimSpace(string(out)))
		return startDirect(cmdPath)
	}

	run := exec.Command("schtasks.exe", "/run", "/tn", taskName)
	run.Dir = exeDir()
	if out, err := run.CombinedOutput(); err != nil {
		log.Printf("[self-update] 触发计划任务失败（%v）：%s；退回直接启动", err, strings.TrimSpace(string(out)))
		return startDirect(cmdPath)
	}
	return nil
}

// startDirect 是退路：直接起一个 cmd。**不可靠**（见 runDetached 的说明），
// 只在 schtasks 用不了的时候兜一手。
func startDirect(cmdPath string) error {
	cmd := exec.Command("cmd.exe", "/c", cmdPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	}
	cmd.Dir = exeDir()
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("无法启动替换脚本：%v", err)
	}
	return nil
}

// buildSelfReplaceScript 生成那个 cmd 脚本。
//
// ★ 头号坑：**cmd 按 ANSI（中文 Windows 上是 GBK）解析 .cmd 文件**，而我们写出去的是
// UTF-8。exe 的目录里含中文（`C:\Users\Administrator\Desktop\家庭日程-服务端-0.2.0\…`），
// 于是写进去的中文在 cmd 眼里全变成 `?`，路径失效、脚本什么都没干（而且不报错，
// 因为 `if exist "C:\…????…"` 只是"条件不成立"）。
//
// 解法**不是**转 GBK —— 那要引 golang.org/x/text，还要处理各种代码页，而且换台机器
// 代码页不同又坏。解法是：**脚本里一个中文都不出现**，路径全部在运行期从自身位置取
// （`%~dp0` = 脚本所在目录 = exe 所在目录）。
//
// 其余几条也是踩过才写上去的，别随手改：
//
//   - 全文没注释：cmd 的注释（`rem`）在括号块里也参与解析，`&` `|` `>` 出现在注释里
//     照样报错。要写说明就写在 Go 这边。
//   - **重定向符前必须留空格**，否则 `...%X%1>nul` 会被解析成变量 `%X%1`。
//   - 用 `setlocal enabledelayedexpansion` + `!var!`，因为要在一个括号块里先算再判。
//   - **`start` 的参数一定要加引号**：`start "t" /b "exe" -addr :8444` 里的 `:8444`
//     会被 start 当成"窗口标题"之类的特殊位置吞掉（实测参数被截成 `-addr`）。
//     写成 `-addr ":8444"` 就好。
//   - **最后一句自删除**：`(goto) 2>nul & del "%~f0"` —— 直接 `del "%~f0"` 会因为
//     "文件正被 cmd 打开"而失败。
//   - 探测用系统自带的 curl.exe（PS 5.1 的 Invoke-WebRequest 在这台机器上跟自签 TLS
//     谈不拢，实测过）。
//
// bakExt 只用来拼备份文件名，形如 `.before-0.1.3`（纯 ASCII）。
func buildSelfReplaceScript(bakExt string) string {
	args := restartArgs()
	exeName := filepath.Base(selfExePath())

	var b strings.Builder
	b.WriteString("@echo off\r\n")
	b.WriteString("setlocal enabledelayedexpansion\r\n")
	b.WriteString("cd /d \"%~dp0\"\r\n")

	// 1. 等旧进程真的退出（最多 30 秒）。计划任务拉起来的进程不会立刻放手文件。
	b.WriteString("set /a n=0\r\n")
	b.WriteString(":waitloop\r\n")
	b.WriteString("set /a n+=1\r\n")
	b.WriteString("(tasklist /fi \"IMAGENAME eq " + exeName + "\" 2>nul | find /i \"" + exeName + "\" >nul) && (\r\n")
	b.WriteString("  if !n! lss 30 (\r\n")
	b.WriteString("    ping -n 2 127.0.0.1 >nul\r\n")
	b.WriteString("    goto waitloop\r\n")
	b.WriteString("  )\r\n")
	b.WriteString(")\r\n")

	// 2. 备份 → 换上去。全程相对自身目录，不出现任何中文。
	b.WriteString("if exist \"" + exeName + "\" copy /y \"" + exeName + "\" \"schedule-server" + bakExt + ".exe\" >nul\r\n")
	b.WriteString("move /y \"" + stagedExeName + "\" \"" + exeName + "\" >nul\r\n")
	b.WriteString("if errorlevel 1 goto rollback\r\n")

	// 3. 起新进程（参数原样带回去）。
	b.WriteString("start \"schedule-server\" /b \"" + exeName + "\"" + args + "\r\n")

	// 4. 探活；探不到就回滚。
	//
	// ⚠️ **判据只能用 curl 的退出码，不要去解析它的输出**。
	//
	// 踩过的坑：原来写成
	//     for /f "delims=" %%i in ('curl -w "%%{http_code}" …') do set OK=%%i
	//     if "!OK!"=="200" goto done
	// 在 cmd 里 `%%{http_code}` 这一串过不了 `for /f` 的解析，`OK` 一直是 0 ——
	// 于是**新版本明明起来了，还是被判成失败、执行回滚**。现象极具欺骗性：
	// 部署时能看到新版本号（那是脚本探活期间我这边探到的），
	// 过一会儿再看又变回旧版本（脚本把备份换回去了）。
	//
	// `curl --fail` 的退出码就够：连不上是非 0、HTTP 4xx/5xx 也是非 0，只有
	// 真正拿到 2xx 才是 0。cmd 里判 `errorlevel` 是最可靠的一件事。
	b.WriteString("set /a m=0\r\n")
	b.WriteString(":probe\r\n")
	b.WriteString("set /a m+=1\r\n")
	b.WriteString("ping -n 2 127.0.0.1 >nul\r\n")
	b.WriteString("\"%SystemRoot%\\System32\\curl.exe\" -skf -o nul --max-time 5 https://127.0.0.1:8443/api/health\r\n")
	b.WriteString("if not errorlevel 1 goto done\r\n")
	b.WriteString("if !m! lss 15 goto probe\r\n")

	// 5. 回滚：把备份换回去，再起一次。
	b.WriteString(":rollback\r\n")
	b.WriteString("echo [%DATE% %TIME%] probe failed, rolling back>> self-update.log\r\n")
	b.WriteString("if not exist \"schedule-server" + bakExt + ".exe\" goto done\r\n")
	b.WriteString("(tasklist /fi \"IMAGENAME eq " + exeName + "\" 2>nul | find /i \"" + exeName + "\" >nul) && taskkill /f /im \"" + exeName + "\" >nul 2>nul\r\n")
	b.WriteString("ping -n 3 127.0.0.1 >nul\r\n")
	b.WriteString("copy /y \"schedule-server" + bakExt + ".exe\" \"" + exeName + "\" >nul\r\n")
	b.WriteString("start \"schedule-server\" /b \"" + exeName + "\"" + args + "\r\n")

	// 走到 done 之前记一行结果 —— 换版本这种"改完就跑、没人盯着"的事，
	// 事后唯一能查的就是这行字。成功那条也记，好和"从没跑过"区分开。
	b.WriteString("goto done\r\n")
	b.WriteString(":done\r\n")
	b.WriteString("echo [%DATE% %TIME%] done>> self-update.log\r\n")
	b.WriteString("endlocal\r\n")
	// 顺手把那个一次性计划任务删掉 —— 它只是用来"脱离本进程树"的，
	// 留着会在任务计划程序里越攒越多。
	b.WriteString("schtasks /delete /tn " + selfUpdateTask + " /f >nul 2>nul\r\n")
	b.WriteString("(goto) 2>nul & del \"%~f0\"\r\n")
	return b.String()
}

// restartArgs 把**本次进程实际用的参数**原样拼回去（不含 exe 路径本身）。
//
// 为什么要原样传：计划任务拉起来的是不带参数的裸 exe（靠默认值），但这次
// 也可能是人手动用 `-addr :8444 -upstream ...` 起的。回不到原样的启动方式 =
// 换完版本服务落在另一个端口上，而健康检查看着"起来了"，最难查。
//
// ⚠️ **不能按"看起来像不像 -flag"去过滤**：flag 和它的值是成对的，
// `-addr :8444` 里 `:8444` 不以 `-` 开头，一过滤就把它扔了 ——
// 生成出来的命令行成了 `"-addr" "-upstream"`，值全丢。整个 `os.Args[1:]`
// 就是最准确的答案，原样搬过去即可。
func restartArgs() string {
	var parts []string
	for _, a := range os.Args[1:] {
		// 替换重启后要回到“管理器”角色，不能把服务核心的标记带过去，
		// 否则新进程不会再拉起面板，而旧管理器还握着已经不存在的子进程。
		if a == "-run-server" || a == "--run-server" {
			continue
		}
		parts = append(parts, quoteArg(a))
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, " ")
}

// quoteArg 给 cmd 的 `start` 用的转义。
//
// **一律加引号**，不做"含空格才加"的省事判断。原因：`start` 对参数的处理很怪，
// 像 `-addr :8444` 这种冒号开头的值会被它当成窗口标题之类的东西吞掉 ——
// 实测生成出来的命令行里 `-addr` 后面直接断掉了。统一加引号就不必去猜
// start 会把哪个字符当特殊含义。
//
// 引号里的 `"` 用 `\"` 转义；我们这里的参数只有端口、路径、URL，够用。
func quoteArg(a string) string {
	return "\"" + strings.ReplaceAll(a, "\"", "\\\"") + "\""
}
