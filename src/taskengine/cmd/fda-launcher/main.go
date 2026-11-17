// Command fda-launcher runs another program as a child process.
//
// macOS privacy controls (TCC) tie Full Disk Access to a specific binary, and a
// rebuilt binary loses the grant. Child processes are attributed to their
// "responsible" parent, so granting Full Disk Access once to this small,
// rarely rebuilt launcher lets the frequently rebuilt taskengine server keep
// access to /Volumes/... across deploys.
//
// Usage: fda-launcher <program> [args...]
// Install it outside the repo's bin/ (e.g. ~/.local/bin) so `make` never
// replaces it.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fda-launcher <program> [args...]")
		os.Exit(2)
	}
	cmd := exec.Command(os.Args[1], os.Args[2:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "fda-launcher: %v\n", err)
		os.Exit(1)
	}

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()

	err := cmd.Wait()
	if ee, ok := err.(*exec.ExitError); ok {
		os.Exit(ee.ExitCode())
	}
	if err != nil {
		os.Exit(1)
	}
}
