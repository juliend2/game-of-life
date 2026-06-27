package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/yourname/gol/internal/spawn"
)

// -- Config -------------------------------------------------------------------

type config struct {
	cellX        int
	cellY        int
	gridWidth    int
	gridHeight   int
	tickInterval time.Duration
	redisAddr    string
	namespace    string
	image        string
	seedMode     bool
	seedCells    []cell
}

type cell struct {
	x, y int
}

func loadConfig() config {
	cfg := config{
		cellX:        mustInt("CELL_X", 0),
		cellY:        mustInt("CELL_Y", 0),
		gridWidth:    mustInt("GRID_WIDTH", 20),
		gridHeight:   mustInt("GRID_HEIGHT", 20),
		tickInterval: time.Duration(mustInt("TICK_INTERVAL", 10)) * time.Second,
		redisAddr:    envOr("REDIS_ADDR", "redis:6379"),
		namespace:    envOr("NAMESPACE", "gol"),
		// The agent needs to know its own image name so it can spawn children
		// with the same image. Injected via the Job template's env.
		image:    envOr("AGENT_IMAGE", "localhost:5000/gol-agent:latest"),
		seedMode: os.Getenv("SEED_MODE") == "true",
	}

	if raw := os.Getenv("SEED_CELLS"); raw != "" {
		cfg.seedCells = parseCells(raw)
	}

	// A tick shorter than ~10s races pod startup: a freshly spawned cell may
	// not have registered itself in Redis yet by the time its neighbors tick.
	if cfg.tickInterval < 10*time.Second {
		log.Printf("WARNING: TICK_INTERVAL=%s is below the recommended 10s floor — "+
			"newly spawned cells may not register in Redis before neighbors evaluate them",
			cfg.tickInterval)
	}

	return cfg
}

// -- Redis helpers ------------------------------------------------------------

// cellKey returns the Redis key for a given cell position.
func cellKey(x, y int) string {
	return fmt.Sprintf("cell:%d:%d", x, y)
}

// isAlive checks whether a cell is registered in Redis.
func isAlive(ctx context.Context, rdb *redis.Client, x, y int) bool {
	val, err := rdb.Exists(ctx, cellKey(x, y)).Result()
	if err != nil {
		log.Printf("redis error checking %d,%d: %v", x, y, err)
		return false
	}
	return val > 0
}

// register marks this cell as alive in Redis.
// The key has no TTL — the cell owns it and removes it on exit.
func register(ctx context.Context, rdb *redis.Client, x, y int) error {
	return rdb.Set(ctx, cellKey(x, y), "1", 0).Err()
}

// deregister removes this cell from Redis.
func deregister(ctx context.Context, rdb *redis.Client, x, y int) {
	if err := rdb.Del(ctx, cellKey(x, y)).Err(); err != nil {
		log.Printf("redis error deregistering %d,%d: %v", x, y, err)
	}
}

// -- GoL logic ----------------------------------------------------------------

// neighbors returns the coordinates of all 8 surrounding cells,
// clamped to the grid boundaries (no wrapping — cells at the edge
// simply have fewer neighbors).
func neighbors(x, y, width, height int) []cell {
	var result []cell
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			if dx == 0 && dy == 0 {
				continue
			}
			nx, ny := x+dx, y+dy
			if nx >= 0 && nx < width && ny >= 0 && ny < height {
				result = append(result, cell{nx, ny})
			}
		}
	}
	return result
}

type tickResult struct {
	survive bool
	spawn   []cell // dead neighbors that should become alive
}

// applyRules evaluates the GoL rules for this cell and its neighborhood.
//
// Standard GoL rules:
//   - A live cell with < 2 live neighbors dies (underpopulation).
//   - A live cell with 2 or 3 live neighbors survives.
//   - A live cell with > 3 live neighbors dies (overpopulation).
//   - A dead cell with exactly 3 live neighbors becomes alive (reproduction).
//
// The agent only handles the first three rules for itself.
// For the fourth rule, it checks each dead neighbor's neighbor count
// and spawns a new Job if the count is exactly 3.
func applyRules(ctx context.Context, rdb *redis.Client, cfg config) tickResult {
	nbrs := neighbors(cfg.cellX, cfg.cellY, cfg.gridWidth, cfg.gridHeight)

	// Count how many of our neighbors are alive.
	aliveCount := 0
	for _, n := range nbrs {
		if isAlive(ctx, rdb, n.x, n.y) {
			aliveCount++
		}
	}

	log.Printf("tick: cell(%d,%d) has %d alive neighbors", cfg.cellX, cfg.cellY, aliveCount)

	result := tickResult{}

	// Apply survival rule for this cell.
	if aliveCount == 2 || aliveCount == 3 {
		result.survive = true
	} else {
		result.survive = false
		return result // dying — no need to check for births
	}

	// Check each dead neighbor for the birth rule.
	// A dead cell is born if it has exactly 3 alive neighbors.
	for _, n := range nbrs {
		if isAlive(ctx, rdb, n.x, n.y) {
			continue // already alive
		}
		// Count alive neighbors of this dead cell.
		deadNbrs := neighbors(n.x, n.y, cfg.gridWidth, cfg.gridHeight)
		deadAliveCount := 0
		for _, dn := range deadNbrs {
			if isAlive(ctx, rdb, dn.x, dn.y) {
				deadAliveCount++
			}
		}
		if deadAliveCount == 3 {
			result.spawn = append(result.spawn, n)
		}
	}

	return result
}

// -- Kubernetes helpers -------------------------------------------------------

func kubeClient() *kubernetes.Clientset {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("failed to get in-cluster kube config: %v", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("failed to create kube client: %v", err)
	}
	return client
}

// spawnConfig translates the agent's config into the shape spawn.Cell wants.
func (c config) spawnConfig() spawn.Config {
	return spawn.Config{
		Namespace:    c.namespace,
		Image:        c.image,
		RedisAddr:    c.redisAddr,
		GridWidth:    c.gridWidth,
		GridHeight:   c.gridHeight,
		TickInterval: c.tickInterval,
	}
}

// trySpawn attempts a Redis SETNX lock then creates the cell Job.
// The lock prevents two surviving neighbors from both creating the same Job
// at the same instant; the deterministic Job name in spawn.Cell handles the
// slower race where one caller's Get happens after the other's Create.
func trySpawn(ctx context.Context, rdb *redis.Client, kube *kubernetes.Clientset, cfg config, x, y int) {
	lockKey := fmt.Sprintf("spawning:%d:%d", x, y)
	set, err := rdb.SetNX(ctx, lockKey, "1", 30*time.Second).Result()
	if err != nil || !set {
		log.Printf("spawn(%d,%d): lock not acquired, another agent is handling it", x, y)
		return
	}
	if err := spawn.Cell(ctx, kube, cfg.spawnConfig(), x, y); err != nil {
		log.Printf("spawn(%d,%d): %v", x, y, err)
		return
	}
	log.Printf("spawn(%d,%d): Job created", x, y)
}

// -- Seed mode ----------------------------------------------------------------

// runSeed creates the initial set of agent Jobs and exits.
// This is what the seeder pod runs (SEED_MODE=true).
// FIXME: Isn't it the job of the Perceiver?
func runSeed(ctx context.Context, kube *kubernetes.Clientset, rdb *redis.Client, cfg config) {
	log.Printf("seed mode: seeding %d cells", len(cfg.seedCells))
	// No SETNX lock needed: the seeder runs once, no other writers.
	for _, c := range cfg.seedCells {
		if err := spawn.Cell(ctx, kube, cfg.spawnConfig(), c.x, c.y); err != nil {
			log.Printf("seed cell(%d,%d): %v", c.x, c.y, err)
		}
	}
	log.Printf("seed mode: done")
}

// -- Main ---------------------------------------------------------------------

func main() {
	cfg := loadConfig()

	log.SetFlags(log.Ltime | log.Lshortfile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle SIGTERM and SIGINT gracefully so the cell deregisters from Redis
	// before exiting. Kubernetes sends SIGTERM before killing the pod.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigs
		log.Printf("received signal %v — shutting down", sig)
		cancel()
	}()

	rdb := redis.NewClient(&redis.Options{
		Addr: cfg.redisAddr,
	})
	defer rdb.Close()

	// Wait for Redis to be reachable before doing anything.
	for {
		if err := rdb.Ping(ctx).Err(); err != nil {
			log.Printf("waiting for Redis at %s: %v", cfg.redisAddr, err)
			time.Sleep(2 * time.Second)
			continue
		}
		break
	}
	log.Printf("connected to Redis at %s", cfg.redisAddr)

	kube := kubeClient()

	if cfg.seedMode {
		runSeed(ctx, kube, rdb, cfg)
		return
	}

	// Normal agent mode: register this cell and run the GoL loop.
	log.Printf("agent starting: cell(%d,%d) on a %dx%d grid, tick=%s",
		cfg.cellX, cfg.cellY, cfg.gridWidth, cfg.gridHeight, cfg.tickInterval)

	if err := register(ctx, rdb, cfg.cellX, cfg.cellY); err != nil {
		log.Fatalf("failed to register in Redis: %v", err)
	}
	log.Printf("cell(%d,%d): registered in Redis", cfg.cellX, cfg.cellY)

	// Always deregister when we exit, whatever the reason.
	defer func() {
		log.Printf("cell(%d,%d): deregistering from Redis", cfg.cellX, cfg.cellY)
		deregister(context.Background(), rdb, cfg.cellX, cfg.cellY)
	}()

	ticker := time.NewTicker(cfg.tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("cell(%d,%d): context cancelled", cfg.cellX, cfg.cellY)
			return

		case <-ticker.C:
			result := applyRules(ctx, rdb, cfg)

			if !result.survive {
				log.Printf("cell(%d,%d): dying", cfg.cellX, cfg.cellY)
				return
			}

			for _, c := range result.spawn {
				trySpawn(ctx, rdb, kube, cfg, c.x, c.y)
			}
		}
	}
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

func parseCells(raw string) []cell {
	var cells []cell
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		coords := strings.SplitN(part, ",", 2)
		if len(coords) != 2 {
			log.Fatalf("invalid cell spec %q (expected x,y)", part)
		}
		x, err1 := strconv.Atoi(strings.TrimSpace(coords[0]))
		y, err2 := strconv.Atoi(strings.TrimSpace(coords[1]))
		if err1 != nil || err2 != nil {
			log.Fatalf("invalid cell spec %q", part)
		}
		cells = append(cells, cell{x, y})
	}
	return cells
}
