package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// gpuRetryAfter is how long a GPU a model failed to start on is skipped before it is tried again.
const gpuRetryAfter = int64(10 * 60)

// errNoGPU means none of a model's GPUs can be provisioned right now.
var errNoGPU = errors.New("no GPU available")

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
		modelStore.setDeployment(m.ID, dep.DeployID, dep.ModelName, m.GPU)
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
	status, err := transition(callbackID, m, dep, time.Now().Unix())
	if err != nil {
		modelStore.setProvisionError(id, err.Error())
	} else {
		modelStore.setProvisionError(id, "")
	}
	return status, err
}

// transition moves a model's deployment toward running. A model runs on the cheapest of its GPUs
// that DeepInfra has available; a GPU it fails to start on is skipped for gpuRetryAfter, so repeated
// calls walk down the list until one starts or none is left.
func transition(callbackID string, m *Model, dep *diDeployment, now int64) (string, error) {
	if dep == nil {
		gpus, err := availableGPUs(callbackID, m, now)
		if err != nil {
			return "", err
		}
		return createOn(callbackID, m, gpus, now)
	}

	modelStore.setDeployment(m.ID, dep.DeployID, dep.ModelName, m.GPU)
	_, runStart := modelStore.activity(m.ID)

	switch {
	case dep.Status == "failed":
		// A failed deployment can't serve and holds the name; replace it on the next GPU.
		failedOn(m, dep, now)
		if delErr := diDelete(callbackID, dep.DeployID); delErr != nil {
			return "", delErr
		}
		gpus, err := availableGPUs(callbackID, m, now)
		if err != nil {
			return "", err
		}
		return createOn(callbackID, m, gpus, now)
	case dep.stopped():
		if dep.startFailed(runStart) {
			failedOn(m, dep, now)
			modelStore.clearRunning(m.ID)
		}
		gpus, err := availableGPUs(callbackID, m, now)
		if err != nil {
			return "", err
		}
		if len(gpus) == 0 {
			return "", noGPUError(m, now) // keep the stopped deployment for when its GPU frees up
		}
		if gpus[0] == m.GPU {
			startErr := diStart(callbackID, dep.DeployID)
			if startErr == nil {
				modelStore.markRunning(m.ID, now)
				return "deploying", nil
			}
			hostLog("warn", fmt.Sprintf("deepinfra-autoscale: start %s on %s failed: %v", m.ID, m.GPU, startErr))
			modelStore.markGPUFailed(m.ID, m.GPU, now)
			if gpus = gpus[1:]; len(gpus) == 0 {
				return "", noGPUError(m, now)
			}
		}
		// Its GPU is unavailable or no longer the cheapest: replace it rather than restart it.
		if delErr := diDelete(callbackID, dep.DeployID); delErr != nil {
			return "", delErr
		}
		return createOn(callbackID, m, gpus, now)
	case dep.Status == "running":
		if runStart == 0 {
			modelStore.markRunning(m.ID, now)
		}
		return "running", nil
	default:
		return dashboardStatus(dep), nil
	}
}

// availableGPUs returns the model's GPUs that DeepInfra has available right now and that it hasn't
// recently failed to start on, cheapest first. It reads availability fresh, not from the cache.
func availableGPUs(callbackID string, m *Model, now int64) ([]string, error) {
	resetGPUCache()
	catalog, err := diGPUs(callbackID)
	if err != nil {
		return nil, fmt.Errorf("read GPU availability: %w", err)
	}
	return rankGPUs(m.GPUs, catalog, modelStore.recentlyFailed(m.ID, now)), nil
}

// createOn creates the model's deployment on the first of gpus that DeepInfra accepts.
func createOn(callbackID string, m *Model, gpus []string, now int64) (string, error) {
	if len(gpus) == 0 {
		return "", noGPUError(m, now)
	}
	var rejected []string
	for _, gpu := range gpus {
		created, err := diCreate(callbackID, m, gpu)
		if err != nil {
			hostLog("warn", fmt.Sprintf("deepinfra-autoscale: create %s on %s failed: %v", m.ID, gpu, err))
			rejected = append(rejected, gpu+": "+err.Error())
			continue
		}
		modelStore.setDeployment(m.ID, created.DeployID, created.ModelName, gpu)
		modelStore.markRunning(m.ID, now)
		return dashboardStatus(created), nil
	}
	return "", fmt.Errorf("could not create %s on any of its GPUs: %s", m.ID, strings.Join(rejected, "; "))
}

// failedOn records that a model's deployment failed to start on its GPU.
func failedOn(m *Model, dep *diDeployment, now int64) {
	modelStore.markGPUFailed(m.ID, m.GPU, now)
	hostLog("warn", fmt.Sprintf("deepinfra-autoscale: %s failed on %s (%s); trying its next GPU", m.ID, m.GPU, dep.FailReason))
}

func noGPUError(m *Model, now int64) error {
	failed := modelStore.recentlyFailed(m.ID, now)
	var unavailable, failedNames []string
	for _, gpu := range m.GPUs {
		if failed[gpu] {
			failedNames = append(failedNames, gpu)
		} else {
			unavailable = append(unavailable, gpu)
		}
	}
	var parts []string
	if len(unavailable) > 0 {
		parts = append(parts, "DeepInfra has none free of "+strings.Join(unavailable, ", "))
	}
	if len(failedNames) > 0 {
		parts = append(parts, fmt.Sprintf("%s just failed to start (retried after %d min)", strings.Join(failedNames, ", "), gpuRetryAfter/60))
	}
	return fmt.Errorf("%w for %s: %s", errNoGPU, m.ID, strings.Join(parts, "; "))
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
	autoscaleWG   sync.WaitGroup
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
	autoscaleWG.Add(1)
	go func() {
		defer autoscaleWG.Done()
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

// stopAutoscaler stops the ticker and waits for its goroutine to fully exit. It is called only from
// the plugin.quiesce RPC handler (host alive), so the goroutine can never run after the plugin's
// .so is detached/unloaded, which would crash.
func stopAutoscaler() {
	autoscaleMu.Lock()
	if !autoscaleOn {
		autoscaleMu.Unlock()
		return
	}
	autoscaleOn = false
	close(autoscaleStop)
	autoscaleMu.Unlock()
	autoscaleWG.Wait()
}

// stuckProvisionLimit is how long a deployment may sit provisioning (normally ~12 minutes) before
// it is treated as stuck and deleted, so a failed start can't bill forever.
const stuckProvisionLimit = int64(2 * 60 * 60)

// runIdleSweep scales to zero any running model idle past the window, deletes deployments stuck
// provisioning, and (on a list outage) stops believed-running models blind by their recorded
// deploy id so an outage can't pin GPUs on. A model in use is never stopped.
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

	all, err := diList("")
	if err != nil {
		sweepFallback(models, now, idleCutoff)
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

		// A model in use whose GPU failed moves down its GPU list without waiting for a request.
		if (dep.Status == "failed" || dep.startFailed(runStart)) && lastUsed > 0 && now-lastUsed <= idleCutoff {
			if _, err := ensureRunningLocked("", m.ID); err != nil {
				hostLog("warn", fmt.Sprintf("deepinfra-autoscale: provision %s: %v", m.ID, err))
			}
			continue
		}

		// Stuck provisioning: a deployment that never reaches running can't be stopped (DeepInfra
		// rejects /stop on non-running), so delete it once it passes stuckProvisionLimit.
		if isProvisioning(dep.Status) {
			if runStart > 0 && now-runStart > stuckProvisionLimit {
				deleteStuck(m.ID, dep.DeployID)
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

		if shouldStop(lastUsed, runStart, now, idleCutoff) {
			stopIdle(m.ID, dep.DeployID, idleCutoff)
		}
	}
}

func isProvisioning(status string) bool {
	return status == "deploying" || status == "downloading" || status == "initializing"
}

// deleteStuck deletes a deployment stuck provisioning past the backstop, under the per-id lock with
// a re-check so it can't delete a deployment that a concurrent wake just replaced.
func deleteStuck(id, deployID string) {
	unlock := modelStore.lock(id)
	defer unlock()
	m, ok := modelStore.get(id)
	if !ok || m.DeployID != deployID {
		return // model removed or deployment replaced concurrently
	}
	dep, err := diFind("", m)
	if err != nil || dep == nil || dep.Status == "running" || dep.stopped() {
		return // recovered or gone
	}
	if err := diDelete("", deployID); err == nil {
		modelStore.clearRunning(id)
		hostLog("warn", fmt.Sprintf("deepinfra-autoscale: deleted %s, stuck %s for over 2h", id, dep.Status))
	}
}

// shouldStop decides whether a running model has been idle past the window and should scale to zero.
func shouldStop(lastUsed, runStart, now, idleCutoff int64) bool {
	idle := lastUsed > 0 && now-lastUsed > idleCutoff
	neverUsed := lastUsed == 0 && runStart > 0 && now-runStart > idleCutoff // woken, never used
	return idle || neverUsed
}

// stopIdle stops one model, re-checking idleness under the per-id lock so it can't race a wake that
// arrived just before.
func stopIdle(id, deployID string, idleCutoff int64) {
	unlock := modelStore.lock(id)
	defer unlock()
	lastUsed, runStart := modelStore.activity(id)
	if !shouldStop(lastUsed, runStart, time.Now().Unix(), idleCutoff) {
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
func sweepFallback(models []*Model, now, idleCutoff int64) {
	hostLog("warn", "deepinfra-autoscale: deploy/list failed; stopping idle models by recorded deploy id")
	for _, m := range models {
		if m.DeployID == "" {
			continue
		}
		lastUsed, runStart := modelStore.activity(m.ID)
		if runStart == 0 {
			continue // not believed running
		}
		if shouldStop(lastUsed, runStart, now, idleCutoff) {
			if err := diStop("", m.DeployID); err == nil {
				modelStore.clearRunning(m.ID)
			}
		}
	}
}
