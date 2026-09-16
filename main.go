package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"time"

	_ "time/tzdata" // embed the tz database so the binary is self-contained
)

const version = "0.4.0"

var errForbidden = errors.New("forbidden")

type Config struct {
	Addr       string
	HTTPAddr   string
	CertDir    string
	DataDir    string
	InviteCode string

	// Weather. The coordinates and the identifying User-Agent stay configurable
	// because met.no rejects anonymous callers and the contact address should be
	// whoever runs the deployment (-place / -lat / -lon / -ua).
	WeatherPlace string
	WeatherLat   float64
	WeatherLon   float64
	WeatherUA    string

	// Diag exposes /api/diag/* and pprof. Off unless -pprof is passed.
	Diag bool
}

type Server struct {
	db      *DB
	cfg     Config
	weather *weatherCache
}

func main() {
	addr := flag.String("addr", ":8443", "HTTPS listen address (empty to disable)")
	httpAddr := flag.String("http", "", "plain HTTP listen address, e.g. :8080 (empty to disable)")
	dataDir := flag.String("data", "./data", "data directory")
	certDir := flag.String("certs", "./certs", "directory holding server.crt / server.key")
	invite := flag.String("invite", "", "invite code required to register (empty = reuse/generate)")
	enablePprof := flag.Bool("pprof", false, "expose /debug/pprof (diagnostics only)")
	place := flag.String("place", defaultWeatherPlace, "weather location name shown to clients")
	lat := flag.Float64("lat", defaultWeatherLat, "weather latitude")
	lon := flag.Float64("lon", defaultWeatherLon, "weather longitude")
	ua := flag.String("ua", "3e-schedule-server/"+version+" (self-hosted calendar; contact: you@example.com)",
		"User-Agent sent to api.met.no — they reject requests without one")
	flag.Parse()

	cfg := Config{
		Addr: *addr, HTTPAddr: *httpAddr, DataDir: *dataDir, CertDir: *certDir, InviteCode: *invite,
		WeatherPlace: *place, WeatherLat: *lat, WeatherLon: *lon, WeatherUA: *ua,
		Diag: *enablePprof,
	}

	db, err := openDB(filepath.Join(cfg.DataDir, "schedule.db"))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	s := &Server{db: db, cfg: cfg, weather: newWeatherCache(cfg.DataDir)}

	// An empty invite code would silently let anyone register, so persist a
	// generated one and print it once.
	if cfg.InviteCode == "" {
		code, _ := db.getMeta("invite_code")
		if code == "" {
			code = newToken()[:10]
			_ = db.setMeta("invite_code", code)
		}
		s.cfg.InviteCode = code
	} else {
		_ = db.setMeta("invite_code", cfg.InviteCode)
	}

	var users int
	_ = db.sql.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users)

	mux := http.NewServeMux()
	s.routes(mux)
	if *enablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		log.Printf("  pprof       : enabled")
	}

	log.Printf("3e-schedule-server %s", version)
	log.Printf("  data dir    : %s", cfg.DataDir)
	log.Printf("  cert dir    : %s", cfg.CertDir)
	log.Printf("  invite code : %s", s.cfg.InviteCode)
	if users == 0 {
		log.Printf("  FIRST RUN   : no accounts yet — the first registration becomes admin")
	}

	handler := withCORS(withLogging(mux))

	if cfg.Addr != "" {
		certFile := filepath.Join(cfg.CertDir, "server.crt")
		keyFile := filepath.Join(cfg.CertDir, "server.key")
		if fileExists(certFile) && fileExists(keyFile) {
			ln, err := net.Listen("tcp", cfg.Addr)
			if err != nil {
				log.Fatalf("listen %s: %v", cfg.Addr, err)
			}
			tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
			srv := &http.Server{
				Handler:           handler,
				TLSConfig:         tlsCfg,
				ReadHeaderTimeout: 15 * time.Second,
			}
			log.Printf("  HTTPS       : %s", cfg.Addr)
			go func() {
				if err := srv.ServeTLS(ln, certFile, keyFile); err != nil {
					log.Fatalf("https serve: %v", err)
				}
			}()
		} else {
			log.Printf("  HTTPS       : skipped (missing %s / %s)", certFile, keyFile)
		}
	}

	if cfg.HTTPAddr != "" {
		log.Printf("  HTTP        : %s", cfg.HTTPAddr)
		srv := &http.Server{Addr: cfg.HTTPAddr, Handler: handler, ReadHeaderTimeout: 15 * time.Second}
		log.Fatal(srv.ListenAndServe())
	}
	select {}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/bootstrap", s.handleBootstrap)

	mux.HandleFunc("POST /api/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.auth(s.handleLogout))
	mux.HandleFunc("GET /api/me", s.auth(s.handleMe))
	mux.HandleFunc("POST /api/password", s.auth(s.handleChangePassword))
	mux.HandleFunc("GET /api/users", s.auth(s.handleUsers))
	mux.HandleFunc("GET /api/invite", s.auth(s.handleGetInvite))

	mux.HandleFunc("GET /api/calendars", s.auth(s.handleListCalendars))
	mux.HandleFunc("POST /api/calendars", s.auth(s.handleCreateCalendar))
	mux.HandleFunc("PUT /api/calendars/{id}", s.auth(s.handleUpdateCalendar))
	mux.HandleFunc("DELETE /api/calendars/{id}", s.auth(s.handleDeleteCalendar))
	// 成员：单个增删（老接口，桌面端/安卓端/AI 都在用）+ 一次性提交（权限页）
	mux.HandleFunc("POST /api/calendars/{id}/members", s.auth(s.handleAddMember))
	mux.HandleFunc("DELETE /api/calendars/{id}/members/{uid}", s.auth(s.handleRemoveMember))
	mux.HandleFunc("PUT /api/calendars/{id}/permissions", s.auth(s.handleSetPermissions))
	mux.HandleFunc("POST /api/calendars/{id}/share", s.auth(s.handleSetCalendarShared))
	mux.HandleFunc("POST /api/calendars/{id}/leave", s.auth(s.handleLeaveCalendar))

	mux.HandleFunc("POST /api/events", s.auth(s.handleCreateEvent))
	mux.HandleFunc("GET /api/events/{id}", s.auth(s.handleGetEvent))
	mux.HandleFunc("PUT /api/events/{id}", s.auth(s.handleUpdateEvent))
	mux.HandleFunc("DELETE /api/events/{id}", s.auth(s.handleDeleteEvent))
	mux.HandleFunc("PUT /api/events/{id}/occurrence", s.auth(s.handleUpsertOccurrence))
	mux.HandleFunc("DELETE /api/events/{id}/occurrence", s.auth(s.handleDeleteOccurrence))
	mux.HandleFunc("POST /api/events/{id}/snooze", s.auth(s.handleSnooze))
	mux.HandleFunc("DELETE /api/events/{id}/snooze", s.auth(s.handleClearSnooze))

	mux.HandleFunc("GET /api/instances", s.auth(s.handleListInstances))

	mux.HandleFunc("GET /api/sync/changes", s.auth(s.handleSyncChanges))
	mux.HandleFunc("GET /api/sync/snapshot", s.auth(s.handleSyncSnapshot))
	mux.HandleFunc("POST /api/sync/push", s.auth(s.handleSyncPush))

	mux.HandleFunc("GET /api/conflicts", s.auth(s.handleListConflicts))
	mux.HandleFunc("POST /api/conflicts/{id}/resolve", s.auth(s.handleResolveConflict))

	// ---------- API keys for machines ----------
	mux.HandleFunc("GET /api/apikeys", s.auth(s.handleListAPIKeys))
	mux.HandleFunc("POST /api/apikeys", s.auth(s.handleCreateAPIKey))
	mux.HandleFunc("DELETE /api/apikeys/{id}", s.auth(s.handleRevokeAPIKey))

	// ---------- open API (session token or API key) ----------
	//
	// Same handlers as the internal routes on purpose: an API key resolves to a
	// user id, so there is exactly one implementation of every operation.
	ext := s.authOrKey
	mux.HandleFunc("GET /api/ext/whoami", ext(s.handleExtWhoami))
	mux.HandleFunc("GET /api/ext/calendars", ext(s.handleListCalendars))
	mux.HandleFunc("GET /api/ext/agenda", ext(s.handleExtAgenda))
	mux.HandleFunc("GET /api/ext/events", ext(normalizeExtQuery(s.handleListInstances)))
	mux.HandleFunc("POST /api/ext/events", ext(s.normalizeExtBody(s.handleCreateEvent)))
	mux.HandleFunc("PUT /api/ext/events/{id}", ext(s.normalizeExtBody(s.handleUpdateEvent)))
	mux.HandleFunc("DELETE /api/ext/events/{id}", ext(s.handleDeleteEvent))
	mux.HandleFunc("PUT /api/ext/events/{id}/occurrence", ext(s.handleUpsertOccurrence))
	mux.HandleFunc("DELETE /api/ext/events/{id}/occurrence", ext(s.handleDeleteOccurrence))
	mux.HandleFunc("GET /api/ext/weather", ext(s.handleWeather))
	mux.HandleFunc("GET /api/ext/openapi.json", ext(s.handleExtOpenAPI))
	mux.HandleFunc("GET /api/ext/guide", ext(s.handleExtGuide))

	// ---------- weather ----------
	mux.HandleFunc("GET /api/weather", ext(s.handleWeather))

	// ---------- hot update ----------
	mux.HandleFunc("GET /api/updates", ext(s.handleUpdateCheck))
	mux.HandleFunc("GET /api/updates/download/{platform}", ext(s.handleUpdateDownload))
	mux.HandleFunc("GET /api/updates/manifest", s.auth(s.handleUpdateManifest))
	mux.HandleFunc("POST /api/updates/publish", s.auth(s.handleUpdatePublish))
	mux.HandleFunc("POST /api/device/report", ext(s.handleDeviceReport))
	mux.HandleFunc("GET /api/devices", s.auth(s.handleDevices))

	// ---------- course timetable ----------
	mux.HandleFunc("GET /api/course/periods", ext(s.handleGetCoursePeriods))
	mux.HandleFunc("PUT /api/course/periods", s.auth(s.handlePutCoursePeriods))

	if s.cfg.Diag {
		mux.HandleFunc("GET /api/diag/weather", s.auth(s.handleWeatherDiag))
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	seq, _ := s.db.currentSeq()
	var users, events, instances int
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users)
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM events WHERE deleted_at IS NULL`).Scan(&events)
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM instances WHERE deleted_at IS NULL`).Scan(&instances)
	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"version":   version,
		"seq":       seq,
		"users":     users,
		"events":    events,
		"instances": instances,
		"time":      time.Now().Unix(),
	})
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/api/health" {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization,Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
