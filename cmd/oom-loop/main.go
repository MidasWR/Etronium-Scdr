// oom-loop: аллоцирует память блоками пока не сожрёт всю доступную.
// Используется для теста cgroup OOM — должен получить OOMKill.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	blockMB := flag.Int("block-mb", 10, "block size in MB")
	limitMB := flag.Int("limit-mb", 200, "max total MB")
	flag.Parse()

	chunks := [][]byte{}
	totalMB := 0
	for totalMB < *limitMB {
		block := make([]byte, *blockMB*1024*1024)
		// Touch pages (lazy alloc).
		for i := 0; i < len(block); i += 4096 {
			block[i] = 1
		}
		chunks = append(chunks, block)
		totalMB += *blockMB
		fmt.Fprintf(os.Stderr, "allocated %d MB\n", totalMB)
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Println("done, holding memory, exiting")
}