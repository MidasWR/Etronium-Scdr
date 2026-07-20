// zombie-fork: форкает N детей, parent ждёт завершения каждого НЕ через wait().
// Дети должны стать зомби после exit, потому что parent не reaping.
// Используется для теста zombie process cleanup.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"
)

func main() {
	count := flag.Int("count", 10, "number of children to fork")
	flag.Parse()

	for i := 0; i < *count; i++ {
		cmd := exec.Command("/bin/true")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "fork #%d failed: %v\n", i, err)
			continue
		}
		// Do NOT call cmd.Wait() — children становятся зомби.
		fmt.Fprintf(os.Stderr, "forked child pid=%d (not waiting)\n", cmd.Process.Pid)
	}

	// Держим parent alive, чтобы зомби жили.
	fmt.Fprintf(os.Stderr, "parent sleeping 30s, leaving children as zombies\n")
	time.Sleep(30 * time.Second)
	fmt.Println("parent exiting")
}