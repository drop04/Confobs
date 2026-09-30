package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"confobs"
)

type Config struct {
	DatabaseURL    string        `env:"DATABASE_URL,required" sensitive:"true"`
	APIKey         string        `env:"API_KEY,required" sensitive:"true"`
	Port           int           `env:"PORT" default:"8080"`
	LogLevel       string        `env:"LOG_LEVEL" default:"info"`
	RequestTimeout time.Duration `env:"REQUEST_TIMEOUT" default:"10s"`
	MaxConns       int           `env:"MAX_CONNS" default:"20"`
}

const (
	envFilePath  = ".env"
	snapshotPath = "./config-snapshot.json"
)

type Server struct {
	mu    sync.RWMutex
	cfg   Config
	tasks []string
}

func (s *Server) config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Server) setConfig(cfg Config) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

func main() {
	if err := loadEnvFile(envFilePath); err != nil && !os.IsNotExist(err) {
		log.Fatalf("reading %s: %v", envFilePath, err)
	}

	var cfg Config
	result, err := confobs.Load(&cfg)
	if err != nil {
		log.Println("startup config problems:")
		for _, m := range result.Missing {
			log.Printf("  missing required env var: %s", m)
		}
		for _, t := range result.Typos {
			log.Printf("  %s", t)
		}
		os.Exit(1)
	}

	if unused, err := confobs.CheckUnused(&cfg, envFilePath); err == nil {
		for _, u := range unused {
			log.Printf("warning: %s is set in %s but not used by any config field", u, envFilePath)
		}
	}

	drift, _, err := confobs.Reload(&cfg, snapshotPath)
	if err != nil {
		log.Fatalf("establishing config snapshot: %v", err)
	}
	if drift.FirstLoad {
		log.Println("no prior config snapshot found — baseline written")
	}

	srv := &Server{cfg: cfg, tasks: []string{}}
	log.Printf("config loaded: port=%d log_level=%s max_conns=%d request_timeout=%s",
		cfg.Port, cfg.LogLevel, cfg.MaxConns, cfg.RequestTimeout)

	go watchForReload(srv)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/tasks", srv.handleTasks)

	initial := srv.config()
	httpServer := &http.Server{
		Addr:         fmt.Sprintf(":%d", initial.Port),
		Handler:      mux,
		ReadTimeout:  initial.RequestTimeout,
		WriteTimeout: initial.RequestTimeout,
	}

	log.Printf("listening on %s — send SIGHUP after editing %s to hot-reload config", httpServer.Addr, envFilePath)
	log.Fatal(httpServer.ListenAndServe())
}

func watchForReload(srv *Server) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP)

	for range sigCh {
		if err := loadEnvFile(envFilePath); err != nil {
			log.Printf("reload: reading %s: %v", envFilePath, err)
			continue
		}

		var newCfg Config
		result, err := confobs.Load(&newCfg)
		if err != nil {
			log.Println("reload: config problems, keeping previous config:")
			for _, m := range result.Missing {
				log.Printf("  missing required env var: %s", m)
			}
			for _, t := range result.Typos {
				log.Printf("  %s", t)
			}
			continue
		}

		drift, _, err := confobs.Reload(&newCfg, snapshotPath)
		if err != nil {
			log.Printf("reload: drift check failed: %v", err)
			continue
		}

		srv.setConfig(newCfg)

		if !drift.Changed() {
			log.Println("reload: config re-read, no changes detected")
			continue
		}
		log.Println("reload: config changed:")
		for _, c := range drift.Changes {
			log.Printf("  %s: %q -> %q", c.Field, c.Old, c.New)
		}
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	cfg := s.config()
	s.mu.RLock()
	taskCount := len(s.tasks)
	s.mu.RUnlock()

	writeJSON(w, map[string]any{
		"status":          "ok",
		"log_level":       cfg.LogLevel,
		"max_conns":       cfg.MaxConns,
		"request_timeout": cfg.RequestTimeout.String(),
		"task_count":      taskCount,
	})
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		tasks := append([]string(nil), s.tasks...)
		s.mu.RUnlock()
		writeJSON(w, map[string]any{"tasks": tasks})

	case http.MethodPost:
		title := r.URL.Query().Get("title")
		if title == "" {
			http.Error(w, "missing ?title=", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.tasks = append(s.tasks, title)
		s.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"added": title})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		os.Setenv(strings.TrimSpace(key), strings.TrimSpace(val))
	}
	return scanner.Err()
}
