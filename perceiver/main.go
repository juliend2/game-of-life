package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/yourname/gol/internal/spawn"
)

// -- Config -------------------------------------------------------------------

type cell struct {
	x, y int
}

type pattern struct {
	name  string
	cells []cell
}

type config struct {
	gridWidth            int
	gridHeight           int
	tickInterval         time.Duration
	redisAddr            string
	namespace            string
	agentImage           string
	extinctionGraceTicks int
	patterns             []pattern
}

func loadConfig() config {
	cfg := config{
		gridWidth:            mustInt("GRID_WIDTH", 20),
		gridHeight:           mustInt("GRID_HEIGHT", 20),
		tickInterval:         time.Duration(mustInt("TICK_INTERVAL", 10)) * time.Second,
		redisAddr:            envOr("REDIS_ADDR", "redis:6379"),
		namespace:            envOr("NAMESPACE", "gol"),
		agentImage:           envOr("AGENT_IMAGE", "localhost:5000/gol-agent:latest"),
		extinctionGraceTicks: mustInt("EXTINCTION_GRACE_TICKS", 3),
		patterns:             parsePatterns(os.Getenv("SEED_PATTERNS")),
	}

	// Fallback pattern if none are configured: a simple glider.
	if len(cfg.patterns) == 0 {
		cfg.patterns = []pattern{
			{
				name: "glider",
				cells: []cell{
					{10, 9}, {11, 10}, {9, 11}, {10, 11}, {11, 11},
				},
			},
		}
	}

	return cfg
}

// -- World state --------------------------------------------------------------

// worldState is the perceiver's in-memory snapshot of the grid.
// It is updated by the watch loop and read by the HTTP handlers.
type worldState struct {
	mu           sync.RWMutex
	alive        map[string]cell // key → cell, matches Redis keys
	lastExtinct  time.Time       // when we last saw an empty grid
	extinctTicks int             // consecutive empty polls
	generation   int             // incremented on each re-seed
	lastReseed   time.Time       // when we last re-seeded
	lastPattern  string          // name of last seeded pattern
}

func newWorldState() *worldState {
	return &worldState{
		alive: make(map[string]cell),
	}
}

func (w *worldState) update(cells map[string]cell) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.alive = cells
}

func (w *worldState) snapshot() map[string]cell {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make(map[string]cell, len(w.alive))
	for k, v := range w.alive {
		out[k] = v
	}
	return out
}

// -- Redis helpers ------------------------------------------------------------

// scanAlive returns all currently alive cells from Redis.
func scanAlive(ctx context.Context, rdb *redis.Client) (map[string]cell, error) {
	keys, err := rdb.Keys(ctx, "cell:*").Result()
	if err != nil {
		return nil, err
	}

	cells := make(map[string]cell, len(keys))
	for _, k := range keys {
		var x, y int
		if _, err := fmt.Sscanf(k, "cell:%d:%d", &x, &y); err != nil {
			continue
		}
		cells[k] = cell{x, y}
	}
	return cells, nil
}

// -- Kubernetes helpers -------------------------------------------------------

func kubeClient() *kubernetes.Clientset {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("failed to get in-cluster kube config: %v", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("failed to create kube client: %v", err)
	}
	return client
}

// spawnConfig translates the perceiver's config into the shape spawn.Cell wants.
func (c config) spawnConfig() spawn.Config {
	return spawn.Config{
		Namespace:    c.namespace,
		Image:        c.agentImage,
		RedisAddr:    c.redisAddr,
		GridWidth:    c.gridWidth,
		GridHeight:   c.gridHeight,
		TickInterval: c.tickInterval,
	}
}

// seed picks a random pattern and spawns all its cells.
func seed(ctx context.Context, kube *kubernetes.Clientset, world *worldState, cfg config) {
	p := cfg.patterns[rand.Intn(len(cfg.patterns))]

	world.mu.Lock()
	world.generation++
	world.lastReseed = time.Now()
	world.lastPattern = p.name
	world.extinctTicks = 0
	world.mu.Unlock()

	log.Printf("perceiver: extinction detected — seeding pattern %q (generation %d)", p.name, world.generation)

	for _, c := range p.cells {
		if err := spawn.Cell(ctx, kube, cfg.spawnConfig(), c.x, c.y); err != nil {
			log.Printf("perceiver: failed to spawn cell(%d,%d): %v", c.x, c.y, err)
		}
	}
}

// -- Watch loop ---------------------------------------------------------------

// watch polls Redis on every tick, updates world state,
// and triggers re-seeding when extinction is detected.
func watch(ctx context.Context, rdb *redis.Client, kube *kubernetes.Clientset, world *worldState, cfg config) {
	// Poll at half the tick interval so we catch changes promptly
	// without hammering Redis.
	pollInterval := cfg.tickInterval / 2
	if pollInterval < time.Second {
		pollInterval = time.Second
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			cells, err := scanAlive(ctx, rdb)
			if err != nil {
				log.Printf("perceiver: redis scan error: %v", err)
				continue
			}

			world.update(cells)

			if len(cells) == 0 {
				world.mu.Lock()
				world.extinctTicks++
				ticks := world.extinctTicks
				world.mu.Unlock()

				log.Printf("perceiver: grid empty (silent tick %d/%d)",
					ticks, cfg.extinctionGraceTicks)

				if ticks >= cfg.extinctionGraceTicks {
					seed(ctx, kube, world, cfg)
				}
			} else {
				world.mu.Lock()
				world.extinctTicks = 0
				world.mu.Unlock()
			}
		}
	}
}

// -- HTTP handlers ------------------------------------------------------------

// GET /grid
// Returns the current grid as a 2D ASCII representation.
// Alive cells are "O", dead cells are ".".
// Also available as JSON with ?format=json.
func handleGrid(world *worldState, cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cells := world.snapshot()

		if r.URL.Query().Get("format") == "json" {
			type jsonCell struct {
				X int `json:"x"`
				Y int `json:"y"`
			}
			var list []jsonCell
			for _, c := range cells {
				list = append(list, jsonCell{c.x, c.y})
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(list)
			return
		}

		// ASCII grid — plain text, readable with curl or watch.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")

		// Build a set for O(1) lookup.
		alive := make(map[[2]int]bool, len(cells))
		for _, c := range cells {
			alive[[2]int{c.x, c.y}] = true
		}

		var sb strings.Builder

		world.mu.RLock()
		gen := world.generation
		pattern := world.lastPattern
		reseed := world.lastReseed
		world.mu.RUnlock()

		fmt.Fprintf(&sb, "generation: %d  last pattern: %s  last reseed: %s\n",
			gen, pattern, reseed.Format(time.RFC3339))
		fmt.Fprintf(&sb, "alive cells: %d\n\n", len(cells))

		// Column header.
		sb.WriteString("   ")
		for x := 0; x < cfg.gridWidth; x++ {
			sb.WriteString(fmt.Sprintf("%2d", x%10))
		}
		sb.WriteString("\n")

		for y := 0; y < cfg.gridHeight; y++ {
			fmt.Fprintf(&sb, "%2d ", y)
			for x := 0; x < cfg.gridWidth; x++ {
				if alive[[2]int{x, y}] {
					sb.WriteString(" O")
				} else {
					sb.WriteString(" .")
				}
			}
			sb.WriteString("\n")
		}

		fmt.Fprint(w, sb.String())
	}
}

// GET /status
// Returns perceiver metadata as JSON — useful for debugging.
func handleStatus(world *worldState, cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		world.mu.RLock()
		status := map[string]any{
			"alive_cells":            len(world.alive),
			"generation":             world.generation,
			"last_pattern":           world.lastPattern,
			"last_reseed":            world.lastReseed,
			"extinct_ticks":          world.extinctTicks,
			"extinction_grace_ticks": cfg.extinctionGraceTicks,
			"grid_width":             cfg.gridWidth,
			"grid_height":            cfg.gridHeight,
			"tick_interval_seconds":  cfg.tickInterval.Seconds(),
		}
		world.mu.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	}
}

// GET /healthz
// Liveness and readiness probe endpoint.
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}

// -- Main ---------------------------------------------------------------------

func main() {
	log.SetFlags(log.Ltime | log.Lshortfile)

	cfg := loadConfig()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigs
		log.Printf("perceiver: received %v — shutting down", sig)
		cancel()
	}()

	rdb := redis.NewClient(&redis.Options{Addr: cfg.redisAddr})
	defer rdb.Close()

	// Wait for Redis.
	for {
		if err := rdb.Ping(ctx).Err(); err != nil {
			log.Printf("perceiver: waiting for Redis at %s: %v", cfg.redisAddr, err)
			time.Sleep(2 * time.Second)
			continue
		}
		break
	}
	log.Printf("perceiver: connected to Redis at %s", cfg.redisAddr)

	kube := kubeClient()
	world := newWorldState()

	// Start the watch loop in the background.
	go watch(ctx, rdb, kube, world, cfg)

	// HTTP server.
	mux := http.NewServeMux()
	mux.HandleFunc("/grid", handleGrid(world, cfg))
	mux.HandleFunc("/status", handleStatus(world, cfg))
	mux.HandleFunc("/healthz", handleHealthz)

	srv := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	// Shut down the HTTP server when context is cancelled.
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("perceiver: listening on :8080")
	log.Printf("perceiver: watching %dx%d grid, extinction grace = %d ticks",
		cfg.gridWidth, cfg.gridHeight, cfg.extinctionGraceTicks)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("perceiver: http server error: %v", err)
	}

	log.Printf("perceiver: goodbye")
}

// -- Util ---------------------------------------------------------------------

func mustInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("invalid value for %s: %q", key, v)
	}
	return n
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parsePatterns parses the SEED_PATTERNS env var.
//
// Format:  name=x,y;x,y;x,y|name=x,y;x,y
// Example: glider=10,9;11,10;9,11|blinker=10,10;10,11;10,12
func parsePatterns(raw string) []pattern {
	var patterns []pattern
	for _, entry := range strings.Split(raw, "|") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			log.Printf("perceiver: skipping malformed pattern entry: %q", entry)
			continue
		}
		name := strings.TrimSpace(parts[0])
		var cells []cell
		for _, coord := range strings.Split(parts[1], ";") {
			coord = strings.TrimSpace(coord)
			if coord == "" {
				continue
			}
			xy := strings.SplitN(coord, ",", 2)
			if len(xy) != 2 {
				log.Printf("perceiver: skipping malformed coord %q in pattern %q", coord, name)
				continue
			}
			x, err1 := strconv.Atoi(strings.TrimSpace(xy[0]))
			y, err2 := strconv.Atoi(strings.TrimSpace(xy[1]))
			if err1 != nil || err2 != nil {
				log.Printf("perceiver: skipping malformed coord %q in pattern %q", coord, name)
				continue
			}
			cells = append(cells, cell{x, y})
		}
		if len(cells) > 0 {
			patterns = append(patterns, pattern{name: name, cells: cells})
		}
	}
	return patterns
}
