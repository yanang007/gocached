// drive runs each command N times per round, rounds interleaved (round 0 is
// a discarded warm-up), and prints the median of the round means, the child
// CPU time, and how often the command failed to start.
// Usage: drive ROUNDS N 'name=cmd|arg|arg' ...
package main

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

type cmdSpec struct {
	name    string
	argv    []string
	ms      []float64
	cpu     time.Duration
	runs    int
	fail    int
	lastErr error
}

func main() {
	rounds, _ := strconv.Atoi(os.Args[1])
	n, _ := strconv.Atoi(os.Args[2])
	var cs []*cmdSpec
	for _, a := range os.Args[3:] {
		name, cmd, _ := strings.Cut(a, "=")
		cs = append(cs, &cmdSpec{name: name, argv: strings.Split(cmd, "|")})
	}
	for r := 0; r <= rounds; r++ {
		for _, x := range cs {
			t := time.Now()
			for i := 0; i < n; i++ {
				cmd := exec.Command(x.argv[0], x.argv[1:]...)
				err := cmd.Run()
				if r == 0 {
					continue
				}
				if cmd.ProcessState == nil {
					x.fail++
					x.lastErr = err
					continue
				}
				x.runs++
				x.cpu += cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
			}
			if r > 0 {
				x.ms = append(x.ms, float64(time.Since(t).Microseconds())/1000/float64(n))
			}
		}
	}
	failed := false
	for _, x := range cs {
		failed = failed || x.fail > 0
		sort.Float64s(x.ms)
		cpu := 0.0
		if x.runs > 0 {
			cpu = float64(x.cpu.Microseconds()) / 1000 / float64(x.runs)
		}
		fmt.Printf("%-24s %7.2f ms (min %.2f, max %.2f)  cpu %.2f ms", x.name, x.ms[len(x.ms)/2], x.ms[0], x.ms[len(x.ms)-1], cpu)
		if x.fail > 0 {
			fmt.Printf("  START FAILED %d/%d: %v", x.fail, x.fail+x.runs, x.lastErr)
		}
		fmt.Println()
	}
	if failed {
		os.Exit(1)
	}
}
