package main

import (
	"fmt"
	"sync"
	"time"
)

// ensureRunning makes a model's deployment exist and be scaled up, returning the dashboard-facing
// status. It never blocks on the (~12 min) cold start; callers fail fast while warming.
//
// The already-running case takes no per-id lock, so concurrent requests to a hot model are not
// serialized. Any state-changing transition (create / start / replace-failed) is done under
// modelStore.lock(id) with a re-check, so it cannot race a concurrent remove into an orphaned
// (billing-but-untracked) deployment.
func ensureRunning(callbackID, id string) (string, error) {
	m, ok := modelStore.get(id)
	if !ok {
		return "", fmt.Errorf("model %q not found", id)
	}
	dep, err := diFind(callbackID, m)
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	if dep != nil && dep.Status == "running" {
		modelStore.setDeployment(m.ID, dep.DeployID, dep.ModelName)
		if _, runStart := modelStore.activity(m.ID); runStart == 0 {
			modelStore.markRunning(m.ID, now)
		}
		return "running", nil
	}
	return ensureRunningLocked(callbackID, id)
}

// ensureRunningLocked performs the provisioning transition under the per-id lock, re-checking that
// the model still exists and re-reading its deployment so it serializes safely against remove.
func ensureRunningLocked(callbackID, id string) (string, error) {
	unlock := modelStore.lock(id)
	defer unlock()

	m, ok := modelStore.get(id)
	if !ok {
		return "", fmt.Errorf("model %q was removed", id)
	}
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

	modelStore.setDeployment(m.ID, dep.DeployID, dep.ModelName)

	switch {
	case dep.Status == "failed":
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
	case dep.stopped():
		if startErr := diStart(callbackID, dep.DeployID); startErr != nil {
			return "", startErr
		}
		modelStore.markRunning(m.ID, now)
		return "deploying", nil
	case dep.Status == "running":
		if _, runStart := modelStore.activity(m.ID); runStart == 0 {
			modelStore.markRunning(m.ID, now)
		}
		return "running", nil
	default:
		return dashboardStatus(dep), nil
	}
}

// wakeModel starts a model and records activity (used by the dashboard wake action).
func wakeModel(callbackID, id string) error {
	if !modelStore.has(id) {
		return fmt.Errorf("model %q not found", id)
	}
	if _, err := ensureRunning(callbackID, id); err != nil {
		return err
	}
	modelStore.markUsed(id, time.Now().Unix())
	return nil
}

// sleepModel scales a model to zero immediately.
func sleepModel(callbackID, id string) error {
	unlock := modelStore.lock(id)
	defer unlock()
	m, ok := modelStore.get(id)
	if !ok {
		return fmt.Errorf("model %q not found", id)
	}
	dep, err := diFind(callbackID, m)
	if err != nil {
		return err
	}
	if dep == nil || dep.stopped() {
		modelStore.clearRunning(id)
		return nil // already gone or stopped
	}
	if err := diStop(callbackID, dep.DeployID); err != nil {
		return err
	}
	modelStore.clearRunning(id)
	return nil
}

// deleteDeployment tears down a model's DeepInfra deployment (used on model removal). It deletes by
// the recorded deploy id when known (authoritative even if the deployment hasn't appeared in
// /deploy/list yet), so a just-created deployment can't be orphaned by list lag.
func deleteDeployment(callbackID string, m *Model) error {
	if m.DeployID != "" {
		if err := diDelete(callbackID, m.DeployID); err != nil {
			return err
		}
		return nil
	}
	dep, err := diFind(callbackID, m)
	if err != nil {
		return err
	}
	if dep == nil {
		return nil
	}
	return diDelete(callbackID, dep.DeployID)
}

// --- idle autoscaler (restartable) ---

var (
	autoscaleMu   sync.Mutex
	autoscaleOn   bool
	autoscaleStop chan struct{}
)

const autoscaleInterval = 60 * time.Second

func startAutoscaler() {
	autoscaleMu.Lock()
	defer autoscaleMu.Unlock()
	if autoscaleOn {
		return
	}
	autoscaleOn = true
	stop := make(chan struct{})
	autoscaleStop = stop
	go func() {
		ticker := time.NewTicker(autoscaleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				runIdleSweep()
			}
		}
	}()
}

func stopAutoscaler() {
	autoscaleMu.Lock()
	defer autoscaleMu.Unlock()
	if !autoscaleOn {
		return
	}
	autoscaleOn = false
	close(autoscaleStop)
}

// shutdown is the process-exit hook. The host may already be tearing down, so it only stops the
// local ticker; the quiesce/shutdown ABI handler does the best-effort remote stop while the host
// is still alive (see stopAllRunning).
func shutdown() { stopAutoscaler() }

// stopAllRunning best-effort stops every model we believe is running, so taking CPA down (quiesce)
// doesn't leave GPUs billing. Called from the quiesce/shutdown ABI handler while the host can still
// service outbound calls.
func stopAllRunning() {
	for _, m := range modelStore.list() {
		if m.DeployID == "" {
			continue
		}
		if _, runStart := modelStore.activity(m.ID); runStart == 0 {
			continue
		}
		if err := diStop("", m.DeployID); err != nil {
			hostLog("warn", fmt.Sprintf("deepinfra-autoscale: shutdown stop %s: %v", m.ID, err))
			continue
		}
		modelStore.clearRunning(m.ID)
	}
}

// runIdleSweep scales to zero any running model idle past the window, deletes stuck provisioning
// deployments past the max-uptime backstop, and (on a list outage) stops believed-running models
// blind by their recorded deploy id so an outage can't pin GPUs on.
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
	now := time.Now().Unix()
	idleCutoff := int64(cfg.IdleMinutes) * 60
	maxUptime := int64(cfg.MaxUptimeMinutes) * 60

	all, err := diList("")
	if err != nil {
		sweepFallback(models, now, idleCutoff, maxUptime)
		return
	}

	byID := map[string]*diDeployment{}
	for i := range all {
		for _, m := range models {
			if all[i].matchesModel(m) {
				byID[m.ID] = &all[i]
			}
		}
	}

	for _, m := range models {
		dep := byID[m.ID]
		if dep == nil {
			continue
		}
		lastUsed, runStart := modelStore.activity(m.ID)

		// Stuck provisioning: a deployment that never reaches running can't be stopped (DeepInfra
		// rejects /stop on non-running), so delete it once it blows the max-uptime backstop.
		if dep.Status == "deploying" || dep.Status == "downloading" || dep.Status == "initializing" {
			if runStart > 0 && now-runStart > maxUptime {
				if err := diDelete("", dep.DeployID); err == nil {
					modelStore.clearRunning(m.ID)
					hostLog("warn", fmt.Sprintf("deepinfra-autoscale: deleted stuck %s (%s > max-uptime)", m.ID, dep.Status))
				}
			}
			continue
		}
		if dep.Status != "running" {
			continue
		}

		// Orphan adoption: a running deployment we have no activity record for (plugin restarted
		// while it was up) gets its clock started now, bounded by the idle window from first sight.
		if lastUsed == 0 && runStart == 0 {
			modelStore.markRunning(m.ID, now)
			continue
		}

		if shouldStop(lastUsed, runStart, now, idleCutoff, maxUptime) {
			stopIdle(m.ID, dep.DeployID, now, idleCutoff, maxUptime)
		}
	}
}

// shouldStop decides whether a running model is idle/over-backstop and should scale to zero.
func shouldStop(lastUsed, runStart, now, idleCutoff, maxUptime int64) bool {
	idle := lastUsed > 0 && now-lastUsed > idleCutoff
	neverUsed := lastUsed == 0 && runStart > 0 && now-runStart > idleCutoff // woken, never used
	overMax := runStart > 0 && now-runStart > maxUptime
	return idle || neverUsed || overMax
}

// stopIdle stops one model, re-checking idleness under the per-id lock so it can't race a wake that
// arrived just before.
func stopIdle(id, deployID string, now, idleCutoff, maxUptime int64) {
	unlock := modelStore.lock(id)
	defer unlock()
	lastUsed, runStart := modelStore.activity(id)
	if !shouldStop(lastUsed, runStart, time.Now().Unix(), idleCutoff, maxUptime) {
		return // a wake landed; leave it running
	}
	if err := diStop("", deployID); err != nil {
		hostLog("warn", fmt.Sprintf("deepinfra-autoscale: stop %s failed: %v", id, err))
		return
	}
	modelStore.clearRunning(id)
	hostLog("info", fmt.Sprintf("deepinfra-autoscale: stopped %s (idle)", id))
}

// sweepFallback runs when /deploy/list is unavailable: stop every believed-running, idle model by
// its recorded deploy id, so a listing outage cannot keep GPUs billing. diStop on an
// already-stopped deployment just errors and is ignored.
func sweepFallback(models []*Model, now, idleCutoff, maxUptime int64) {
	hostLog("warn", "deepinfra-autoscale: deploy/list failed; stopping idle models by recorded deploy id")
	for _, m := range models {
		if m.DeployID == "" {
			continue
		}
		lastUsed, runStart := modelStore.activity(m.ID)
		if runStart == 0 {
			continue // not believed running
		}
		if shouldStop(lastUsed, runStart, now, idleCutoff, maxUptime) {
			if err := diStop("", m.DeployID); err == nil {
				modelStore.clearRunning(m.ID)
			}
		}
	}
}
