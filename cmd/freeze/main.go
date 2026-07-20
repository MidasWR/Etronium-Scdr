// etronium-freeze — test binary for lord.Freeze / Thaw.
//
// USAGE:
//   etronium-freeze --pid=<PID> --dir=/tmp/chk
//
// Spawns a fresh "sleep" via exec.Cmd and freezes the *spawned* child
// (not what --pid points at, since that wouldn't be a child of the
// test binary and would not be ptraceable by us without elevated caps).
//
// TODO: do actual remote-freeze on --pid by going through lord RPC.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/midas/Etronium-Scdr/internal/lord"
)

func main() {
	var (
		chkDir = flag.String("dir", "/tmp/etronium-freeze-test", "checkpoint directory")
		hold   = flag.Int("hold", 60, "seconds for sleep")
		kill   = flag.Bool("kill-source", false, "kill the source process after Freeze (simulates migration)")
	)
	flag.Parse()

	if err := os.MkdirAll(*chkDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir:", err)
		os.Exit(1)
	}

	sleepCmd := exec.Command("sleep", fmt.Sprint(*hold))
	sleepCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // NB: Setsid=true triggers EPERM under privileged Docker 6.x kernel.
	sleepCmd.Stdout = nil
	sleepCmd.Stderr = nil
	if err := sleepCmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start sleep:", err)
		os.Exit(1)
	}
	pid := sleepCmd.Process.Pid
	fmt.Fprintf(os.Stderr, "spawned sleep pid=%d\n", pid)
	time.Sleep(500 * time.Millisecond) // let it enter syscall(2)

	t := time.Now()
	res, err := lord.Freeze(pid, *chkDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Freeze failed:", err)
		_ = sleepCmd.Wait()
		os.Exit(2)
	}
	fmt.Printf("Freeze OK in %v: pages=%d fds=%d maps=%d\n",
		time.Since(t).Round(time.Millisecond),
		res.PagesWritten, res.FdsCaptured, res.MemoryMapSize)

	// List checkpoint contents.
	entries, _ := os.ReadDir(*chkDir)
	fmt.Printf("Checkpoint directory: %s\n", *chkDir)
	for _, e := range entries {
		fi, _ := e.Info()
		if fi != nil {
			fmt.Printf("  %-30s %d bytes\n", e.Name(), fi.Size())
		} else {
			fmt.Printf("  %s\n", e.Name())
		}
	}

	// Print maps.txt for human inspection.
	mapsPath := filepath.Join(*chkDir, "maps.txt")
	if data, err := os.ReadFile(mapsPath); err == nil {
		fmt.Printf("\n--- /proc/%d/maps ---\n", pid)
		fmt.Print(string(data))
	}

	if *kill {
		fmt.Println("\n--- DetachKill (kill source) ---")
		if err := lord.DetachKill(pid); err != nil {
			fmt.Fprintln(os.Stderr, "DetachKill:", err)
		}
	} else {
		fmt.Println("\n--- Thaw (resume source) ---")
		if err := lord.Thaw(pid); err != nil {
			fmt.Fprintln(os.Stderr, "Thaw:", err)
		}
	}

	// Wait for sleep to finish naturally.
	_ = sleepCmd.Wait()
	fmt.Println("\nsleep exited, test done")
}
