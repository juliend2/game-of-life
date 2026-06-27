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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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

// jobName returns the deterministic Job name for a cell.
// Kubernetes job names must be lowercase alphanumeric + hyphens.
func jobName(x, y int) string {
	return fmt.Sprintf("gol-cell-%d-%d", x, y)
}

// spawnCell creates a Kubernetes Job for a new cell at (x, y).
// If a Job already exists there (another agent beat us to it), it does nothing.
func spawnCell(ctx context.Context, kube *kubernetes.Clientset, cfg config, x, y int) {
	name := jobName(x, y)

	// Use SETNX on Redis as a lightweight lock to prevent two agents
	// from spawning the same cell simultaneously.
	// SETNX = SET if Not eXists — atomic in Redis.
	rdb := redis.NewClient(&redis.Options{Addr: cfg.redisAddr})
	defer rdb.Close()

	lockKey := fmt.Sprintf("spawning:%d:%d", x, y)
	set, err := rdb.SetNX(ctx, lockKey, "1", 30*time.Second).Result()
	if err != nil || !set {
		log.Printf("spawn(%d,%d): lock not acquired, another agent is handling it", x, y)
		return
	}

	// Check if the Job already exists in Kubernetes.
	_, err = kube.BatchV1().Jobs(cfg.namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		log.Printf("spawn(%d,%d): Job already exists", x, y)
		return
	}
	if !errors.IsNotFound(err) {
		log.Printf("spawn(%d,%d): kube error: %v", x, y, err)
		return
	}

	backoffLimit := int32(0)
	xStr := strconv.Itoa(x)
	yStr := strconv.Itoa(y)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.namespace,
			Labels: map[string]string{
				"app":   "gol-agent",
				"gol-x": xStr,
				"gol-y": yStr,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":   "gol-agent",
						"gol-x": xStr,
						"gol-y": yStr,
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "gol-agent",
					RestartPolicy:      corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:            "agent",
							Image:           cfg.image,
							ImagePullPolicy: corev1.PullAlways,
							Env: []corev1.EnvVar{
								{Name: "CELL_X", Value: xStr},
								{Name: "CELL_Y", Value: yStr},
								{Name: "REDIS_ADDR", Value: cfg.redisAddr},
								{Name: "AGENT_IMAGE", Value: cfg.image},
								{Name: "NAMESPACE", Value: cfg.namespace},
								{Name: "GRID_WIDTH", Value: strconv.Itoa(cfg.gridWidth)},
								{Name: "GRID_HEIGHT", Value: strconv.Itoa(cfg.gridHeight)},
								{Name: "TICK_INTERVAL", Value: strconv.Itoa(int(cfg.tickInterval.Seconds()))},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: mustQuantity("16Mi"),
									corev1.ResourceCPU:    mustQuantity("10m"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: mustQuantity("32Mi"),
									corev1.ResourceCPU:    mustQuantity("100m"),
								},
							},
						},
					},
				},
			},
		},
	}

	_, err = kube.BatchV1().Jobs(cfg.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		log.Printf("spawn(%d,%d): failed to create Job: %v", x, y, err)
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
	for _, c := range cfg.seedCells {
		spawnCell(ctx, kube, cfg, c.x, c.y)
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
				spawnCell(ctx, kube, cfg, c.x, c.y)
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

// mustQuantity parses a Kubernetes resource quantity string.
// Panics on invalid input — these are hardcoded constants, not user input.
func mustQuantity(s string) resource.Quantity {
	return resource.MustParse(s)
}
