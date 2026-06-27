// Package spawn builds the Kubernetes Job that runs a cell agent.
// Shared by both the agent (when a surviving cell spawns its dead neighbors)
// and the perceiver (when re-seeding after extinction).
package spawn

import (
	"context"
	"fmt"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Config carries the bits of caller config that influence the shape of
// spawned cell Jobs (image, env values inherited by the child, etc.).
type Config struct {
	Namespace    string
	Image        string
	RedisAddr    string
	GridWidth    int
	GridHeight   int
	TickInterval time.Duration
}

// JobName returns the deterministic Kubernetes Job name for a cell.
// Determinism is load-bearing: two concurrent callers racing to spawn the
// same dead neighbor will collide on the name, so the loser hits
// IsAlreadyExists rather than creating a duplicate Job.
func JobName(x, y int) string {
	return fmt.Sprintf("gol-cell-%d-%d", x, y)
}

// Cell creates a Kubernetes Job for a new cell at (x, y).
// Idempotent: returns nil if a Job with the deterministic name already exists.
func Cell(ctx context.Context, kube kubernetes.Interface, cfg Config, x, y int) error {
	name := JobName(x, y)

	_, err := kube.BatchV1().Jobs(cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("kube get %s: %w", name, err)
	}

	backoffLimit := int32(0)
	xStr := strconv.Itoa(x)
	yStr := strconv.Itoa(y)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cfg.Namespace,
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
							Image:           cfg.Image,
							ImagePullPolicy: corev1.PullAlways,
							Env: []corev1.EnvVar{
								{Name: "CELL_X", Value: xStr},
								{Name: "CELL_Y", Value: yStr},
								{Name: "REDIS_ADDR", Value: cfg.RedisAddr},
								{Name: "AGENT_IMAGE", Value: cfg.Image},
								{Name: "NAMESPACE", Value: cfg.Namespace},
								{Name: "GRID_WIDTH", Value: strconv.Itoa(cfg.GridWidth)},
								{Name: "GRID_HEIGHT", Value: strconv.Itoa(cfg.GridHeight)},
								{Name: "TICK_INTERVAL", Value: strconv.Itoa(int(cfg.TickInterval.Seconds()))},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("16Mi"),
									corev1.ResourceCPU:    resource.MustParse("10m"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("32Mi"),
									corev1.ResourceCPU:    resource.MustParse("100m"),
								},
							},
						},
					},
				},
			},
		},
	}

	if _, err := kube.BatchV1().Jobs(cfg.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("kube create %s: %w", name, err)
	}
	return nil
}
