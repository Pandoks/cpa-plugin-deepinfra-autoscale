package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ensureRunning makes a model's deployment exist and be scaled up. It returns the dashboard-facing
// status after the action. It never blocks on the (~12 min) cold start; callers fail fast.
func ensureRunning(callbackID string, m *Model) (string, error) {
	dep, err := diFind(callbackID, m)
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()

	if dep == nil {
		created, createErr := diCreate(callbackID, m)
		if createErr != nil {
			return "", createErr
		}
		modelStore.setDeployment(m.ID, created.DeployID, created.ModelName)
		modelStore.markRunning(m.ID, now)
		return dashboardStatus(created), nil
	}

	// Keep our linkage fresh (e.g. after a restart that matched by name).
	modelStore.setDeployment(m.ID, dep.DeployID, dep.ModelName)

	if dep.Status == "failed" {
		// A failed deployment can't serve and holds the name; replace it.
		if delErr := diDelete(callbackID, dep.DeployID); delErr != nil {
			return "", delErr
		}
		created, createErr := diCreate(callbackID, m)
		if createErr != nil {
			return "", createErr
		}
		modelStore.setDeployment(m.ID, created.DeployID, created.ModelName)
		modelStore.markRunning(m.ID, now)
		return dashboardStatus(created), nil
	}

	if dep.Settings != nil && dep.Settings.MinInstances == 0 {
		if scaleErr := diScale(callbackID, dep.DeployID, 1); scaleErr != nil {
			return "", scaleErr
		}
		modelStore.markRunning(m.ID, now)
		return "deploying", nil
	}

	if dep.Status == "running" {
		if _, runStart := modelStore.activity(m.ID); runStart == 0 {
			modelStore.markRunning(m.ID, now)
		}
		return "running", nil
	}
	return dashboardStatus(dep), nil
}

// wakeModel starts a model and records activity (used by the dashboard wake action).
func wakeModel(callbackID, id string) error {
	m, ok := modelStore.get(id)
	if !ok {
		return fmt.Errorf("model %q not found", id)
	}
	if _, err := ensureRunning(callbackID, m); err != nil {
		return err
	}
	modelStore.markUsed(id, time.Now().Unix())
	return nil
}

// sleepModel scales a model to zero immediately.
func sleepModel(callbackID, id string) error {
	m, ok := modelStore.get(id)
	if !ok {
		return fmt.Errorf("model %q not found", id)
	}
	dep, err := diFind(callbackID, m)
	if err != nil {
		return err
	}
	if dep == nil {
		return nil // already gone
	}
	if dep.Settings != nil && dep.Settings.MinInstances == 0 {
		modelStore.clearRunning(id)
		return nil
	}
	if err := diScale(callbackID, dep.DeployID, 0); err != nil {
		return err
	}
	modelStore.clearRunning(id)
	return nil
}

// deleteDeployment tears down a model's DeepInfra deployment (used on model removal).
func deleteDeployment(callbackID string, m *Model) error {
	dep, err := diFind(callbackID, m)
	if err != nil {
		return err
	}
	if dep == nil {
		return nil
	}
	return diDelete(callbackID, dep.DeployID)
}

// --- idle autoscaler ---

var (
	autoscaleOnce sync.Once
	autoscaleStop = make(chan struct{})
	stopOnce      sync.Once
)

const autoscaleInterval = 60 * time.Second

func startAutoscaler() {
	autoscaleOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(autoscaleInterval)
			defer ticker.Stop()
			for {
				select {
				case <-autoscaleStop:
					return
				case <-ticker.C:
					runIdleSweep()
				}
			}
		}()
	})
}

func shutdown() {
	stopOnce.Do(func() { close(autoscaleStop) })
}

// runIdleSweep scales to zero any running model idle past the window, or up past the max-uptime
// backstop. One deploy/list call is shared across all models.
func runIdleSweep() {
	defer func() {
		if v := recover(); v != nil {
			hostLog("warn", fmt.Sprintf("deepinfra-autoscale: idle sweep panic: %v", v))
		}
	}()

	cfg := getConfig()
	models := modelStore.list()
	if len(models) == 0 {
		return
	}
	all, err := diList("")
	if err != nil {
		hostLog("warn", fmt.Sprintf("deepinfra-autoscale: idle sweep list failed: %v", err))
		return
	}
	byID := map[string]*diDeployment{}
	for i := range all {
		if diGone[all[i].Status] {
			continue
		}
		dep := &all[i]
		for _, m := range models {
			if dep.DeployID == m.DeployID || dep.ModelName == m.ID || strings.HasSuffix(dep.ModelName, "/"+m.ID) {
				byID[m.ID] = dep
			}
		}
	}

	now := time.Now().Unix()
	idleCutoff := int64(cfg.IdleMinutes) * 60
	maxUptime := int64(cfg.MaxUptimeMinutes) * 60

	for _, m := range models {
		dep := byID[m.ID]
		if dep == nil || dep.Status != "running" {
			continue
		}
		if dep.Settings != nil && dep.Settings.MinInstances == 0 {
			continue
		}
		lastUsed, runStart := modelStore.activity(m.ID)

		idle := lastUsed > 0 && now-lastUsed > idleCutoff
		// Woken but never used within the window: don't pay indefinitely.
		neverUsed := lastUsed == 0 && runStart > 0 && now-runStart > idleCutoff
		overMax := runStart > 0 && now-runStart > maxUptime

		if idle || neverUsed || overMax {
			if err := diScale("", dep.DeployID, 0); err != nil {
				hostLog("warn", fmt.Sprintf("deepinfra-autoscale: scale-to-zero %s failed: %v", m.ID, err))
				continue
			}
			modelStore.clearRunning(m.ID)
			reason := "idle"
			if overMax {
				reason = "max-uptime"
			} else if neverUsed {
				reason = "unused"
			}
			hostLog("info", fmt.Sprintf("deepinfra-autoscale: scaled %s to zero (%s)", m.ID, reason))
		}
	}
}
