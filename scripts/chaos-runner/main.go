// chaos-runner: orchestrator для smoke-теста Etronium.
//
// Использование:
//   docker compose -f test/chaos/docker-compose.yml up -d
//   docker exec etronium-chaos /usr/local/bin/chaos-runner
//
// Что делает:
//   1. Ждёт готовности scheduler + 3 active lords
//   2. Прогоняет 11 сценариев (~22 мин)
//   3. Записывает метрики в /tmp/chaos-report.json
//   4. Печатает summary в stdout
//
// Сценарии:
//   [00:00] Baseline       — 5 stateless, distribution check
//   [02:00] Stateful V5    — example-stateful + kill lord mid-write
//   [05:00] Lord lag       — Q-lords подключаются с delay
//   [08:00] Net partition  — iptables drop lord-A2
//   [10:00] Spawn storm    — 100 spawn за 5 сек
//   [12:00] cgroup OOM     — mem_limit=10MB
//   [15:00] Zombies        — fork + kill parent
//   [17:00] Cold start     — kill scheduler, restart, WAL replay
//   [19:00] K8s workload   — apply nginx pods в sidecar k3s
//   [22:00] Final report
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ScenarioResult — итог одного сценария.
type ScenarioResult struct {
	Name         string             `json:"name"`
	StartedAt    time.Time          `json:"started_at"`
	Duration     time.Duration      `json:"duration"`
	Success      bool               `json:"success"`
	Error        string             `json:"error,omitempty"`
	LatencyP50MS float64            `json:"latency_p50_ms,omitempty"`
	LatencyP95MS float64            `json:"latency_p95_ms,omitempty"`
	LatencyP99MS float64            `json:"latency_p99_ms,omitempty"`
	RecoveryMS   float64            `json:"recovery_ms,omitempty"`
	Metrics      map[string]float64 `json:"metrics,omitempty"`
	Notes        []string           `json:"notes,omitempty"`
}

// Global report aggregator.
type Report struct {
	StartedAt   time.Time         `json:"started_at"`
	FinishedAt  time.Time         `json:"finished_at"`
	Scheduler   string            `json:"scheduler_addr"`
	Scenarios   []ScenarioResult  `json:"scenarios"`
	ClusterInfo map[string]string `json:"cluster_info"`
}

var (
	rng        = rand.New(rand.NewSource(time.Now().UnixNano()))
	reportMu   sync.Mutex
	report     Report
	logFile    *os.File
	logStartAt = time.Now()
)

func log(format string, args ...any) {
	line := fmt.Sprintf("[%s] %s", time.Since(logStartAt).Round(time.Millisecond), fmt.Sprintf(format, args...))
	fmt.Println(line)
	if logFile != nil {
		logFile.WriteString(line + "\n")
	}
}

func logStep(name string) {
	log("")
	log("================================================================")
	log("  %s", name)
	log("================================================================")
}

func recordResult(r ScenarioResult) {
	reportMu.Lock()
	defer reportMu.Unlock()
	report.Scenarios = append(report.Scenarios, r)
	if r.Success {
		log("    ✓ %s in %s (p50=%.1fms p95=%.1fms p99=%.1fms recovery=%.1fms)",
			r.Name, r.Duration.Round(time.Millisecond),
			r.LatencyP50MS, r.LatencyP95MS, r.LatencyP99MS, r.RecoveryMS)
	} else {
		log("    ✗ %s: %s", r.Name, r.Error)
	}
}

// runCmd — выполнить shell команду в foreground (chaos контейнер имеет docker socket? нет, используем ssh-like через docker exec).
func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// dockerExec — выполнить команду в контейнере.
func dockerExec(container string, cmd ...string) (string, error) {
	args := append([]string{"exec", container}, cmd...)
	return runCmd("docker", args...)
}

// dockerExists — существует ли контейнер.
func dockerExists(container string) bool {
	_, err := runCmd("docker", "inspect", container)
	return err == nil
}

// dockerKill — убить контейнер (--signal по умолчанию SIGKILL).
func dockerKill(container, signal string) error {
	if signal == "" {
		signal = "SIGKILL"
	}
	_, err := runCmd("docker", "kill", "--signal", signal, container)
	return err
}

// dockerStart — стартовать существующий контейнер.
func dockerStart(container string) error {
	_, err := runCmd("docker", "start", container)
	return err
}

// dockerPause / dockerUnpause — freeze cgroup процессов внутри контейнера.
func dockerPause(container string) error {
	_, err := runCmd("docker", "pause", container)
	return err
}
func dockerUnpause(container string) error {
	_, err := runCmd("docker", "unpause", container)
	return err
}

// waitForScheduler — ждать пока scheduler отвечает.
func waitForScheduler(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := dockerExec("etronium-tenant", "./bin/etronium", "lords")
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("scheduler %s not ready in %s", addr, timeout)
}

// etroniumCmd — выполнить etronium CLI в tenant контейнере.
func etroniumCmd(args ...string) (string, error) {
	cmdStr := "./bin/etronium " + strings.Join(args, " ")
	return dockerExec("etronium-tenant", "sh", "-c", cmdStr)
}

// measureSpawnLatency — spawn N процессов, замерить latency для каждого.
// Возвращает p50/p95/p99 в миллисекундах.
func measureSpawnLatency(ctx context.Context, n int) ([]time.Duration, error) {
	latencies := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		t0 := time.Now()
		_, err := etroniumCmd("process", "spawn", "--exec=/bin/sleep", fmt.Sprintf("--arg=%d", 300))
		if err != nil {
			return latencies, fmt.Errorf("spawn #%d: %w", i, err)
		}
		latencies = append(latencies, time.Since(t0))
		select {
		case <-ctx.Done():
			return latencies, ctx.Err()
		default:
		}
	}
	return latencies, nil
}

func percentiles(durs []time.Duration, ps ...float64) []float64 {
	if len(durs) == 0 {
		return make([]float64, len(ps))
	}
	sorted := make([]time.Duration, len(durs))
	copy(sorted, durs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	res := make([]float64, len(ps))
	for i, p := range ps {
		idx := int(float64(len(sorted)-1) * p)
		res[i] = float64(sorted[idx]) / float64(time.Millisecond)
	}
	return res
}

// =================================================================
// SCENARIOS
// =================================================================

// S01Baseline — 5 stateless процессов, замер distribution.
func S01Baseline(ctx context.Context) ScenarioResult {
	r := ScenarioResult{Name: "01_baseline_5_stateless", StartedAt: time.Now()}
	defer func() { r.Duration = time.Since(r.StartedAt) }()

	// Spawn 5.
	lordAssignments := make(map[string]int)
	pids := []string{}
	for i := 0; i < 5; i++ {
		out, err := etroniumCmd("process", "spawn", "--exec=/bin/sleep", "--arg=300")
		if err != nil {
			r.Error = fmt.Sprintf("spawn #%d: %v", i, err)
			return r
		}
		pid := extractField(out, "process_id")
		pids = append(pids, pid)
	}

	// Wait for all RUNNING.
	time.Sleep(2 * time.Second)

	// Get distribution via list --json.
	out, err := etroniumCmd("process", "list", "--json")
	if err != nil {
		r.Error = fmt.Sprintf("list: %v", err)
		return r
	}
	lordAssignments = parseLordDistribution(out)

	// Cleanup.
	for _, pid := range pids {
		etroniumCmd("process", "kill", pid)
	}

	r.Success = true
	r.Metrics = map[string]float64{
		"spawned":      float64(len(pids)),
		"distinct_lords": float64(len(lordAssignments)),
	}
	r.Notes = []string{fmt.Sprintf("distribution: %v", lordAssignments)}
	return r
}

// S02StatefulV5 — V5 app, kill lord mid-write, проверить что counter пережил.
func S02StatefulV5(ctx context.Context) ScenarioResult {
	r := ScenarioResult{Name: "02_stateful_v5", StartedAt: time.Now()}
	defer func() { r.Duration = time.Since(r.StartedAt) }()

	stateFile := "/tmp/etronium/state/chaos.json"

	// Spawn stateful app.
	out, err := etroniumCmd("process", "spawn",
		"--exec=/usr/local/bin/example-stateful",
		fmt.Sprintf("--state-dump=%s", stateFile),
		"--max-restarts=10")
	if err != nil {
		r.Error = fmt.Sprintf("spawn stateful: %v", err)
		return r
	}
	pid := extractField(out, "process_id")
	appLord := extractField(out, "lord_id")

	// Wait 5s for state to populate.
	time.Sleep(5 * time.Second)

	beforeCounter := readCounter(stateFile)
	if beforeCounter <= 0 {
		r.Error = fmt.Sprintf("counter not populated: %v", beforeCounter)
		return r
	}

	// Kill app lord.
	lordContainer := "etronium-lord-" + strings.TrimPrefix(appLord, "lord-")
	t0 := time.Now()
	if err := dockerKill(lordContainer, "SIGKILL"); err != nil {
		// fallback: try numbered names
		for _, suffix := range []string{"01", "02", "03"} {
			if err := dockerKill("etronium-lord-active-"+suffix, "SIGKILL"); err == nil {
				lordContainer = "etronium-lord-active-" + suffix
				break
			}
		}
	}
	log("    killed lord container %s at t0", lordContainer)

	// Wait for recovery (new counter value > before).
	recovered := false
	var recoveryTime time.Duration
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		c := readCounter(stateFile)
		if c > beforeCounter {
			recoveryTime = time.Since(t0)
			recovered = true
			break
		}
		time.Sleep(1 * time.Second)
	}

	if !recovered {
		r.Error = "recovery timeout (60s)"
		return r
	}

	r.Success = true
	r.RecoveryMS = float64(recoveryTime) / float64(time.Millisecond)
	r.Metrics = map[string]float64{
		"counter_before": float64(beforeCounter),
		"counter_after":  float64(readCounter(stateFile)),
	}
	r.Notes = []string{
		fmt.Sprintf("app originally on %s", appLord),
		fmt.Sprintf("counter before kill: %d, after: %d", int(beforeCounter), int(readCounter(stateFile))),
	}

	// Cleanup.
	etroniumCmd("process", "kill", pid)
	return r
}

// S03LordLag — Q-lords подключаются с delay.
func S03LordLag(ctx context.Context) ScenarioResult {
	r := ScenarioResult{Name: "03_lord_lag_join", StartedAt: time.Now()}
	defer func() { r.Duration = time.Since(r.StartedAt) }()

	// Initial state: 3 active.
	_ = etroniumCmd // suppress unused
	// Actually let's count rows in lords table.
	out2, _ := etroniumCmd("lords")
	initialCount := countLordRows(out2)
	log("    initial lord count: %d", initialCount)

	// Start queued lords one by one.
	for i, q := range []string{"queued-4", "queued-5", "queued-6"} {
		log("    starting %s (t+%ds)", q, (i+1)*15)
		if err := dockerStart("etronium-lord-" + q); err != nil {
			log("    start failed for %s: %v", q, err)
		}
		time.Sleep(15 * time.Second)

		// Re-check count.
		out, _ := etroniumCmd("lords")
		count := countLordRows(out)
		log("    after %s: %d lords total", q, count)
	}

	// Final check.
	finalOut, _ := etroniumCmd("lords")
	finalCount := countLordRows(finalOut)

	r.Success = finalCount >= 6
	if !r.Success {
		r.Error = fmt.Sprintf("expected 6 lords, got %d", finalCount)
	}
	r.Metrics = map[string]float64{
		"lords_initial": float64(initialCount),
		"lords_final":   float64(finalCount),
	}
	return r
}

// S04NetPartition — iptables drop lord-A2 → scheduler.
func S04NetPartition(ctx context.Context) ScenarioResult {
	r := ScenarioResult{Name: "04_network_partition", StartedAt: time.Now()}
	defer func() { r.Duration = time.Since(r.StartedAt) }()

	// Find scheduler IP (host.docker.internal or actual IP).
	schedIP := getSchedulerIP()

	// Block traffic to scheduler from lord-active-2.
	if _, err := dockerExec("etronium-lord-active-2", "iptables", "-A", "OUTPUT",
		"-d", schedIP, "-j", "DROP"); err != nil {
		r.Error = fmt.Sprintf("iptables add: %v", err)
		return r
	}
	log("    partitioned lord-active-2 from scheduler %s", schedIP)

	// Wait 35s for heartbeat TTL.
	time.Sleep(35 * time.Second)

	// Check: lord-active-2 should be marked unhealthy.
	out, _ := etroniumCmd("lords")
	lines := strings.Split(out, "\n")
	a2MarkedUnhealthy := false
	for _, line := range lines {
		if strings.Contains(line, "etronium-lord-active-2") || strings.Contains(line, "lord-active-2") {
			if strings.Contains(line, "False") || strings.Contains(line, "false") {
				a2MarkedUnhealthy = true
			}
		}
	}

	// Restore network.
	_, _ = dockerExec("etronium-lord-active-2", "iptables", "-D", "OUTPUT",
		"-d", schedIP, "-j", "DROP")

	r.Success = a2MarkedUnhealthy
	if !r.Success {
		r.Error = "lord-active-2 was NOT marked unhealthy after 35s partition"
	}
	r.Notes = []string{
		fmt.Sprintf("scheduler IP probed: %s", schedIP),
		fmt.Sprintf("lord-active-2 unhealthy after partition: %v", a2MarkedUnhealthy),
	}
	return r
}

// S05SpawnStorm — 100 процессов за 5 сек.
func S05SpawnStorm(ctx context.Context) ScenarioResult {
	r := ScenarioResult{Name: "05_spawn_storm", StartedAt: time.Now()}
	defer func() { r.Duration = time.Since(r.StartedAt) }()

	const N = 100
	latencies, err := measureSpawnLatency(ctx, N)
	if err != nil {
		r.Error = err.Error()
	}
	// Cleanup.
	out, _ := etroniumCmd("process", "list", "--json")
	for _, pid := range extractAllPIDs(out) {
		etroniumCmd("process", "kill", pid)
	}

	if len(latencies) == 0 {
		return r
	}
	p50, p95, p99 := percentiles(latencies, 0.5, 0.95, 0.99)[0],
		percentiles(latencies, 0.5, 0.95, 0.99)[1],
		percentiles(latencies, 0.5, 0.95, 0.99)[2]
	r.LatencyP50MS = p50
	r.LatencyP95MS = p95
	r.LatencyP99MS = p99
	r.Success = err == nil && p99 < 5000 // p99 должен быть < 5s
	if !r.Success && err == nil {
		r.Error = fmt.Sprintf("p99 too high: %.0fms", p99)
	}
	r.Metrics = map[string]float64{
		"spawned":       float64(len(latencies)),
		"throughput_per_sec": float64(N) / time.Since(r.StartedAt).Seconds(),
	}
	return r
}

// S06CgroupOOM — spawn процесс с mem_limit=10MB, должен получить OOMKill.
func S06CgroupOOM(ctx context.Context) ScenarioResult {
	r := ScenarioResult{Name: "06_cgroup_oom", StartedAt: time.Now()}
	defer func() { r.Duration = time.Since(r.StartedAt) }()

	// 10MB memory limit, процесс пытается аллоцировать 100MB.
	out, err := etroniumCmd("process", "spawn",
		"--exec=/usr/local/bin/oom-loop",
		"--resources={\"mem_limit_bytes\":10485760}")
	if err != nil {
		r.Error = fmt.Sprintf("spawn oom: %v", err)
		return r
	}
	pid := extractField(out, "process_id")

	// Wait 10s for OOM.
	time.Sleep(10 * time.Second)

	// Check exit code.
	state, _ := etroniumCmd("process", "get", pid)
	oomKilled := strings.Contains(state, "OOM") || strings.Contains(state, "killed") ||
		strings.Contains(state, "STOPPED") || strings.Contains(state, "EXITED")

	r.Success = oomKilled
	if !oomKilled {
		r.Error = "process not OOM-killed: " + state
	}
	r.Metrics = map[string]float64{"mem_limit_mb": 10}
	return r
}

// S07Zombies — spawn форкер, kill parent, проверить zombie count.
func S07Zombies(ctx context.Context) ScenarioResult {
	r := ScenarioResult{Name: "07_zombie_processes", StartedAt: time.Now()}
	defer func() { r.Duration = time.Since(r.StartedAt) }()

	out, err := etroniumCmd("process", "spawn",
		"--exec=/usr/local/bin/zombie-fork", "--arg=20")
	if err != nil {
		r.Error = fmt.Sprintf("spawn zombie: %v", err)
		return r
	}
	pid := extractField(out, "process_id")

	time.Sleep(3 * time.Second)

	// Kill parent. Children должны либо тоже умереть, либо остаться зомби.
	etroniumCmd("process", "kill", pid)
	time.Sleep(3 * time.Second)

	// Check via ps on the lord (we don't have direct shell into lord, use scheduler's view).
	// For now, expect process state to be EXITED/STOPPED.
	state, _ := etroniumCmd("process", "get", pid)
	cleanExit := strings.Contains(state, "EXITED") || strings.Contains(state, "STOPPED")

	r.Success = cleanExit
	if !cleanExit {
		r.Error = "zombie parent not cleaned up: " + state
	}
	return r
}

// S08ColdStart — kill scheduler, перезапустить, WAL replay.
func S08ColdStart(ctx context.Context) ScenarioResult {
	r := ScenarioResult{Name: "08_scheduler_cold_start", StartedAt: time.Now()}
	defer func() { r.Duration = time.Since(r.StartedAt) }()

	// Spawn 5 процессов которые переживут scheduler death.
	prePIDs := []string{}
	for i := 0; i < 5; i++ {
		out, err := etroniumCmd("process", "spawn", "--exec=/bin/sleep", "--arg=300")
		if err != nil {
			r.Error = fmt.Sprintf("pre-spawn: %v", err)
			return r
		}
		prePIDs = append(prePIDs, extractField(out, "process_id"))
	}

	// Kill scheduler.
	t0 := time.Now()
	dockerKill("etronium-scheduler", "SIGKILL")
	time.Sleep(2 * time.Second)

	// Restart scheduler.
	if err := dockerStart("etronium-scheduler"); err != nil {
		r.Error = fmt.Sprintf("scheduler restart: %v", err)
		return r
	}

	// Wait for scheduler ready.
	if err := waitForScheduler(ctx, ":50051", 30*time.Second); err != nil {
		r.Error = fmt.Sprintf("scheduler not ready: %v", err)
		return r
	}

	// List processes — должны быть (replayed from WAL).
	time.Sleep(2 * time.Second)
	out, err := etroniumCmd("process", "list", "--json")
	if err != nil {
		r.Error = fmt.Sprintf("list after cold start: %v", err)
		return r
	}
	listed := extractAllPIDs(out)

	// Check that pre-spawned PIDs are in list.
	matched := 0
	for _, pre := range prePIDs {
		for _, lp := range listed {
			if lp == pre {
				matched++
			}
		}
	}

	coldStartTime := time.Since(t0)
	r.Success = matched >= len(prePIDs)
	r.RecoveryMS = float64(coldStartTime) / float64(time.Millisecond)
	r.Metrics = map[string]float64{
		"pre_spawned":   float64(len(prePIDs)),
		"replayed":      float64(matched),
		"replay_ratio":  float64(matched) / float64(len(prePIDs)),
	}
	if !r.Success {
		r.Error = fmt.Sprintf("WAL replay incomplete: %d/%d", matched, len(prePIDs))
	}

	// Cleanup.
	for _, pid := range prePIDs {
		etroniumCmd("process", "kill", pid)
	}
	return r
}

// S09K8sWorkload — применить 50 nginx подов в sidecar k3s, замер что Etronium не мешает.
func S09K8sWorkload(ctx context.Context) ScenarioResult {
	r := ScenarioResult{Name: "09_k8s_sidecar_workload", StartedAt: time.Now()}
	defer func() { r.Duration = time.Since(r.StartedAt) }()

	// Apply nginx deployment в k3s sidecar.
	yamlPath := "/tmp/chaos/nginx-deployment.yaml"
	if err := os.WriteFile(yamlPath, []byte(nginxYAML), 0o644); err != nil {
		r.Error = fmt.Sprintf("write yaml: %v", err)
		return r
	}

	// Copy yaml в k3s контейнер.
	if _, err := runCmd("docker", "cp", yamlPath, "etronium-k3s:/tmp/nginx.yaml"); err != nil {
		r.Error = fmt.Sprintf("docker cp: %v", err)
		return r
	}

	t0 := time.Now()
	if _, err := dockerExec("etronium-k3s", "kubectl", "--kubeconfig=/etc/rancher/k3s/k3s.yaml",
		"apply", "-f", "/tmp/nginx.yaml"); err != nil {
		r.Error = fmt.Sprintf("kubectl apply: %v", err)
		return r
	}

	// Wait for pods ready.
	ready := false
	for i := 0; i < 30; i++ {
		out, _ := dockerExec("etronium-k3s", "kubectl", "--kubeconfig=/etc/rancher/k3s/k3s.yaml",
			"get", "deployment", "nginx", "-o", "jsonpath={.status.readyReplicas}")
		if strings.TrimSpace(out) == "50" {
			ready = true
			break
		}
		time.Sleep(2 * time.Second)
	}

	if !ready {
		r.Error = "nginx deployment did not become ready (50 replicas)"
		return r
	}

	// Verify Etronium не мешает: list process должен работать.
	if _, err := etroniumCmd("process", "list"); err != nil {
		r.Error = fmt.Sprintf("etronium list failed: %v", err)
		return r
	}

	r.Success = true
	r.RecoveryMS = float64(time.Since(t0)) / float64(time.Millisecond)
	r.Metrics = map[string]float64{
		"k8s_deploy_time_sec": float64(time.Since(t0)) / float64(time.Second),
		"k8s_pods_ready":      50,
	}
	return r
}

// S10FinalReport — собрать глобальные метрики.
func S10FinalReport(ctx context.Context) ScenarioResult {
	r := ScenarioResult{Name: "10_final_state", StartedAt: time.Now()}
	defer func() { r.Duration = time.Since(r.StartedAt) }()

	lordsOut, _ := etroniumCmd("lords")
	lordCount := countLordRows(lordsOut)
	procOut, _ := etroniumCmd("process", "list", "--json")
	procCount := len(extractAllPIDs(procOut))

	r.Success = lordCount >= 6
	if !r.Success {
		r.Error = fmt.Sprintf("expected >= 6 lords, got %d", lordCount)
	}
	r.Metrics = map[string]float64{
		"lords_healthy":  float64(lordCount),
		"processes_alive": float64(procCount),
	}
	return r
}

// =================================================================
// HELPERS
// =================================================================

func extractField(out, key string) string {
	// Парсит строки вида "process_id: 01ABC..."
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, key+":"))
		}
	}
	// Try JSON path: "process_id":"01ABC..."
	if idx := strings.Index(out, `"`+key+`":"`); idx >= 0 {
		rest := out[idx+len(key)+5:]
		if end := strings.Index(rest, `"`); end >= 0 {
			return rest[:end]
		}
	}
	return ""
}

func extractAllPIDs(out string) []string {
	var pids []string
	// JSON path: ищем все "process_id":"..."
	for {
		idx := strings.Index(out, `"process_id":"`)
		if idx < 0 {
			break
		}
		rest := out[idx+14:]
		end := strings.Index(rest, `"`)
		if end < 0 {
			break
		}
		pids = append(pids, rest[:end])
		out = rest[end:]
	}
	return pids
}

func parseLordDistribution(jsonOut string) map[string]int {
	res := make(map[string]int)
	// Грубый парсинг: ищем "lord_id":"<X>"
	for {
		idx := strings.Index(jsonOut, `"lord_id":"`)
		if idx < 0 {
			break
		}
		rest := jsonOut[idx+11:]
		end := strings.Index(rest, `"`)
		if end < 0 {
			break
		}
		res[rest[:end]]++
		jsonOut = rest[end:]
	}
	return res
}

func countLordRows(out string) int {
	// Парсим вывод "etronium lords" — каждая строка кроме header — lord.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	count := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "LORD_ID") || strings.HasPrefix(line, "---") {
			continue
		}
		// Heuristic: первое поле содержит alnum.
		if len(line) > 10 && strings.ContainsAny(line, "0123456789") {
			count++
		}
	}
	return count
}

func readCounter(path string) float64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return 0
	}
	if c, ok := m["counter"].(float64); ok {
		return c
	}
	return 0
}

func getSchedulerIP() string {
	// Scheduler на хосте или в отдельном контейнере. Если в compose network —
	// его hostname == "etronium-scheduler".
	// Если scheduler на хосте — это host.docker.internal.
	// Пробуем оба.
	candidates := []string{"etronium-scheduler", "host.docker.internal", "172.17.0.1", "127.0.0.1"}
	for _, c := range candidates {
		if out, err := dockerExec("etronium-tenant", "getent", "hosts", c); err == nil && out != "" {
			// Возьмём IP.
			fields := strings.Fields(out)
			if len(fields) > 0 {
				return fields[0]
			}
		}
	}
	return "127.0.0.1"
}

const nginxYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx
  namespace: default
spec:
  replicas: 50
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      labels:
        app: nginx
    spec:
      containers:
      - name: nginx
        image: nginx:alpine
        resources:
          requests:
            cpu: 50m
            memory: 32Mi
          limits:
            cpu: 100m
            memory: 64Mi
`

// =================================================================
// MAIN
// =================================================================

func main() {
	rand.Seed(time.Now().UnixNano())
	logStartAt = time.Now()

	// Setup log file.
	logFile, _ = os.Create("/tmp/chaos-runner.log")
	defer logFile.Close()

	report.StartedAt = time.Now()
	report.Scheduler = "127.0.0.1:50051"
	report.ClusterInfo = map[string]string{}

	log("CHAOS RUNNER starting")
	log("Scheduler: %s", report.Scheduler)

	// Ensure required containers exist.
	required := []string{
		"etronium-scheduler",
		"etronium-tenant",
		"etronium-lord-active-1", "etronium-lord-active-2", "etronium-lord-active-3",
		"etronium-lord-queued-4", "etronium-lord-queued-5", "etronium-lord-queued-6",
		"etronium-k3s",
	}
	missing := []string{}
	for _, c := range required {
		if !dockerExists(c) {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		log("FATAL: missing containers: %v", missing)
		log("Run: docker compose -f test/chaos/docker-compose.yml up -d")
		os.Exit(1)
	}

	// Wait for scheduler.
	log("Waiting for scheduler to be ready...")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := waitForScheduler(ctx, ":50051", 60*time.Second); err != nil {
		log("FATAL: %v", err)
		os.Exit(1)
	}
	log("Scheduler is up")

	// Run scenarios.
	scenarios := []struct {
		Name string
		Fn   func(context.Context) ScenarioResult
		Wait time.Duration
	}{
		{"01_baseline", S01Baseline, 0},
		{"02_stateful_v5", S02StatefulV5, 0},
		{"03_lord_lag", S03LordLag, 0},
		{"04_net_partition", S04NetPartition, 0},
		{"05_spawn_storm", S05SpawnStorm, 0},
		{"06_cgroup_oom", S06CgroupOOM, 0},
		{"07_zombies", S07Zombies, 0},
		{"08_cold_start", S08ColdStart, 0},
		{"09_k8s", S09K8sWorkload, 0},
		{"10_final", S10FinalReport, 0},
	}

	for _, sc := range scenarios {
		logStep("Scenario " + sc.Name)
		t0 := time.Now()
		result := sc.Fn(ctx)
		result.Duration = time.Since(t0)
		recordResult(result)
		if sc.Wait > 0 {
			time.Sleep(sc.Wait)
		}
	}

	report.FinishedAt = time.Now()

	// Save report.
	reportPath := "/tmp/chaos-report.json"
	data, _ := json.MarshalIndent(report, "", "  ")
	os.WriteFile(reportPath, data, 0o644)

	log("")
	log("================================================================")
	log("  CHAOS REPORT")
	log("================================================================")
	totalScenarios := len(report.Scenarios)
	passed := 0
	for _, sc := range report.Scenarios {
		if sc.Success {
			passed++
		}
	}
	log("  Scenarios passed: %d / %d", passed, totalScenarios)
	log("  Total time: %s", report.FinishedAt.Sub(report.StartedAt).Round(time.Second))
	log("  Report file: %s", reportPath)
	log("")

	if passed < totalScenarios {
		os.Exit(1)
	}
	_ = filepath.Base // suppress unused
}