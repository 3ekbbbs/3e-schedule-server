package app

// Server 持有进程内所有共享状态。
//
// 没有全局单例是刻意的：测试与多实例（同一进程内起两套）都不需要额外处理。
type Server struct {
	db      *DB
	cfg     Config
	weather *weatherCache
	// words 是违禁词引擎（带内存缓存）。写路径会高频查它，不能每次查库。
	words *wordEngine
	// loginLog 把账号事件写 JSONL 文件（与库双写）。
	loginLog *loginLogLogger
}
