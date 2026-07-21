// Command etronium — CLI клиент tenant'а (арендатора).
//
// Flat subcommand interface — like supervisord/systemd-run:
//
//	etronium run <exec> [args...]            — spawn
//	etronium ps [--running]                  — list this tenant's processes
//	etronium get <pid>                       — get one process state
//	etronium kill <pid> [--signal=N] [--force] — send signal
//	etronium wait <pid> [--timeout=N]        — block until exit
//	etronium status                           — fleet summary
//	etronium lords                            — list registered lords
//	etronium token ...                        — Phase 3+ stub
//	etronium version                          — print CLI version
//
// Все миграции/ребалансы делает autoscale внутри scheduler'а автоматически
// (см. internal/scheduler/autoscale.go). Ручных migrate нет.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	etroniumv1 "github.com/midas/Etronium-Scdr/internal/gen/etronium/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	schedulerAddr string
	tenantID      string
	outputJSON    bool
)

// version is overridden at link time via -ldflags="-X main.version=...".
var version = "dev"

func main() {
	rootCmd := &cobra.Command{
		Use:   "etronium",
		Short: "Etronium-Scdr tenant CLI",
		Long:  "CLI клиент для управления процессами в Single System Image.",
	}

	rootCmd.PersistentFlags().StringVar(&schedulerAddr, "scheduler",
		envOr("ETRONIUM_SCHEDULER", "localhost:51061"), "scheduler gRPC address")
	rootCmd.PersistentFlags().StringVar(&tenantID, "tenant",
		envOr("ETRONIUM_TENANT", "anonymous"), "tenant id (арендатор)")
	rootCmd.PersistentFlags().BoolVar(&outputJSON, "json", false, "JSON output")

	rootCmd.AddCommand(runCmd())
	rootCmd.AddCommand(psCmd())
	rootCmd.AddCommand(getCmd())
	rootCmd.AddCommand(killCmd())
	rootCmd.AddCommand(waitCmd())
	rootCmd.AddCommand(statusCmd())
	rootCmd.AddCommand(lordsCmd())
	rootCmd.AddCommand(tokenCmd())
	rootCmd.AddCommand(formatFleetCmd())
	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print etronium CLI version",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Fprintf(os.Stderr, "etronium %s\n", version)
		},
	})

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// ───────────────────────────────────────────────────────────────────────
// run — spawn a new process. Positional exec + args.
//
//	etronium run /bin/sleep 60
//	etronium run /bin/sh -c "while true; do echo hi; sleep 1; done"
//
// Optional resource/prefer/restart flags before the positional exec:
//
//	etronium run --cpu-shares=100 --mem-mb=100 --max-restarts=10 /bin/sleep 60
func runCmd() *cobra.Command {
	var (
		cpuShares    int32
		memMB        int32
		preferLord   string
		maxRestarts  int32
		stateDump    string
	)
	c := &cobra.Command{
		Use:   "run <exec> [args...]",
		Short: "Spawn a new process",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			execPath := args[0]
			argv := args[1:]

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			client, conn, err := dial(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			req := &etroniumv1.SpawnRequest{
				TenantId:          tenantID,
				ExecPath:          execPath,
				Argv:              argv,
				PreferLordId:      preferLord,
				MaxRestarts:       maxRestarts,
				StateDumpPathHint: stateDump,
			}
			if cpuShares > 0 || memMB > 0 {
				req.Resources = &etroniumv1.ResourceSpec{
					CpuShares:       cpuShares,
					MemLimitBytes:   int64(memMB) * 1024 * 1024,
				}
			}
			info, err := client.Spawn(ctx, req)
			if err != nil {
				return err
			}
			printProcessInfo(info)
			return nil
		},
	}
	c.Flags().Int32Var(&cpuShares, "cpu-shares", 0, "CPU shares (100 = 1 core)")
	c.Flags().Int32Var(&memMB, "mem-mb", 0, "memory limit in MB")
	c.Flags().Int32Var(&maxRestarts, "max-restarts", 10, "max restarts on lord failure (0..N, -1=unlimited)")
	c.Flags().StringVar(&stateDump, "state-dump", "", "V5 application state-dump path")
	c.Flags().StringVar(&preferLord, "prefer-lord", "", "soft-affinity lord id (hint, scheduler may override)")
	return c
}

// ps — list this tenant's processes.
func psCmd() *cobra.Command {
	var onlyRunning bool
	c := &cobra.Command{
		Use:   "ps",
		Short: "List tenant processes",
		Aliases: []string{"list", "ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client, conn, err := dial(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := client.ListProcesses(ctx, &etroniumv1.ListProcessesRequest{
				TenantId:    tenantID,
				OnlyRunning: onlyRunning,
			})
			if err != nil {
				return err
			}
			if outputJSON {
				out, _ := json.MarshalIndent(resp.Processes, "", "  ")
				fmt.Println(string(out))
				return nil
			}
			fmt.Printf("%-26s  %-15s  %-12s  %s\n", "PROCESS_ID", "LORD", "STATE", "EXEC")
			for _, p := range resp.Processes {
				fmt.Printf("%-26s  %-15s  %-12s  %s\n",
					p.Ref.ProcessId, p.Ref.LordId, p.State.String(), p.ExecPath)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&onlyRunning, "running", false, "only RUNNING/PAUSED")
	return c
}

// get — process state for one pid.
func getCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <process_id>",
		Short: "Get process state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client, conn, err := dial(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()
			info, err := client.GetProcess(ctx, &etroniumv1.GetProcessRequest{ProcessId: args[0]})
			if err != nil {
				return err
			}
			printProcessInfo(info)
			return nil
		},
	}
}

// kill — send signal (default SIGTERM=15, --force=SIGKILL=9).
func killCmd() *cobra.Command {
	var (
		signal int32
		force  bool
	)
	c := &cobra.Command{
		Use:   "kill <process_id>",
		Short: "Send signal (default SIGTERM)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client, conn, err := dial(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := client.Kill(ctx, &etroniumv1.KillRequest{
				ProcessId:    args[0],
				SignalNumber: signal,
				Force:        force,
			})
			if err != nil {
				return err
			}
			fmt.Printf("acknowledged=%v state=%s\n", resp.Acknowledged, resp.CurrentState.String())
			return nil
		},
	}
	c.Flags().Int32Var(&signal, "signal", 15, "signal number (default 15 = SIGTERM)")
	c.Flags().BoolVar(&force, "force", false, "SIGKILL (skip grace)")
	return c
}

// wait — block until process exits (returns exit code).
func waitCmd() *cobra.Command {
	var timeoutSec int32
	c := &cobra.Command{
		Use:   "wait <process_id>",
		Short: "Wait for process exit",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec+30)*time.Second)
			defer cancel()
			client, conn, err := dial(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()
			info, err := client.Wait(ctx, &etroniumv1.WaitRequest{
				ProcessId:  args[0],
				TimeoutSec: timeoutSec,
			})
			if err != nil {
				return err
			}
			printProcessInfo(info)
			return nil
		},
	}
	c.Flags().Int32Var(&timeoutSec, "timeout", 60, "timeout in seconds (0 = forever, max 600)")
	return c
}

// lords — list registered lords.
func lordsCmd() *cobra.Command {
	var onlyHealthy bool
	return &cobra.Command{
		Use:   "lords",
		Short: "List registered lords",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client, conn, err := dial(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := client.ListLords(ctx, &etroniumv1.ListLordsRequest{OnlyHealthy: onlyHealthy})
			if err != nil {
				return err
			}
			if outputJSON {
				out, _ := json.MarshalIndent(resp.Lords, "", "  ")
				fmt.Println(string(out))
				return nil
			}
			fmt.Printf("%-26s  %-20s  %-6s  %s\n", "LORD_ID", "HOSTNAME", "CORES", "MEM (MB)")
			for _, l := range resp.Lords {
				fmt.Printf("%-26s  %-20s  %-6d  %d\n",
					l.LordId, l.Hostname, l.CpuCoresPhysical, l.MemTotalBytesPhysical/(1024*1024))
			}
			return nil
		},
	}
}

// status — fleet summary.
func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show fleet status from scheduler (lords + processing).",
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, conn, err := dial(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()
			resp, err := client.ListLords(ctx, &etroniumv1.ListLordsRequest{})
			if err != nil {
				return err
			}
			lords := resp.GetLords()
			out := map[string]interface{}{
				"scheduler": schedulerAddr,
				"tenant":    tenantID,
				"lords":     lords,
				"healthy":   countHealthy(lords),
			}
			if outputJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				_ = enc.Encode(out)
				return nil
			}
			h := out["healthy"].(int)
			fmt.Printf("scheduler: %s\n", schedulerAddr)
			fmt.Printf("lords:     %d (%d healthy)\n", len(lords), h)
			for i, l := range lords {
				fmt.Printf("  [%d] %-20s advertised_cpu=%d host=%s\n",
					i, l.GetHostname(), l.GetAdvertisedCpuShares(), l.GetOs())
			}
			return nil
		},
	}
}

// token — Phase 3+ stub.
func tokenCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "token",
		Short: "Manage tenant access tokens (Phase 3+ stub)",
	}
	c.AddCommand(tokenNewCmd(), tokenListCmd(), tokenRevokeCmd())
	return c
}

func tokenNewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "new",
		Short: "Issue a new tenant token (Phase 3+).",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Fprintln(os.Stderr, "tenant token new: not implemented in Phase 1 (MVP runs without auth).")
		},
	}
}

func tokenListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List tenant tokens (Phase 3+).",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Fprintln(os.Stderr, "tenant token list: not implemented in Phase 1.")
		},
	}
}

func tokenRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke",
		Short: "Revoke a tenant token (Phase 3+).",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Fprintln(os.Stderr, "tenant token revoke: not implemented in Phase 1.")
		},
	}
}

// format-fleet — humanize JSON fleet dump (used by installer.sh status).
func formatFleetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "format-fleet",
		Short: "Read JSON from stdin, print human fleet summary.",
		Run: func(_ *cobra.Command, _ []string) {
			var lords []*etroniumv1.Lord
			if err := json.NewDecoder(os.Stdin).Decode(&lords); err != nil {
				fmt.Fprintf(os.Stderr, "decode: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("%-20s %-12s %-10s %s\n", "HOSTNAME", "CPU-SHARES", "STATUS", "LORD-ID")
			for _, l := range lords {
				fmt.Printf("%-20s %-12d %-10s %s\n",
					l.GetHostname(), l.GetAdvertisedCpuShares(),
					boolStr(l.GetHealthy(), "healthy", "down"),
					l.GetLordId())
			}
		},
	}
}

// ───────────────────────────────────────────────────────────────────────
// helpers

func dial(ctx context.Context) (etroniumv1.SchedulerServiceClient, *grpc.ClientConn, error) {
	conn, err := grpc.NewClient(schedulerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("dial scheduler %s: %w", schedulerAddr, err)
	}
	return etroniumv1.NewSchedulerServiceClient(conn), conn, nil
}

func printProcessInfo(p *etroniumv1.ProcessInfo) {
	if outputJSON {
		out, _ := json.MarshalIndent(p, "", "  ")
		fmt.Println(string(out))
		return
	}
	fmt.Printf("process_id: %s\n", p.Ref.ProcessId)
	fmt.Printf("lord_id:    %s\n", p.Ref.LordId)
	fmt.Printf("local_pid:  %d\n", p.Ref.LocalPid)
	fmt.Printf("state:      %s\n", p.State.String())
	fmt.Printf("exec:       %s %v\n", p.ExecPath, p.Argv)
	if p.ExitCode != 0 || p.ExitSignal != 0 {
		fmt.Printf("exit_code:  %d  exit_signal: %d\n", p.ExitCode, p.ExitSignal)
	}
	if p.MemPeakBytes > 0 {
		fmt.Printf("mem_peak:   %d bytes\n", p.MemPeakBytes)
	}
}

func countHealthy(lords []*etroniumv1.Lord) int {
	n := 0
	for _, l := range lords {
		if l.GetHealthy() {
			n++
		}
	}
	return n
}

func boolStr(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}