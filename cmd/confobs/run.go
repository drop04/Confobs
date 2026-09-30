package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
)

// cmdRun is the zero-code integration: the program being started needs no
// confobs code at all.
//
//  1. Validate the environment against the schema. If anything is wrong, print
//     every problem and do NOT start the program.
//  2. Otherwise start it with an environment in which every schema default has
//     been filled in, so the program can read PORT (say) without carrying its
//     own fallback and the default lives in exactly one place: the schema.
func cmdRun(args []string, stdout, stderr io.Writer) int {
	var c commonFlags
	var quiet bool
	var snapshotPath string
	fs := newFlagSet("run", stderr)
	c.register(fs)
	fs.StringVar(&snapshotPath, "snapshot", "", "report drift since the last run, then update this baseline")
	fs.BoolVar(&quiet, "quiet", false, "suppress confobs's own messages")
	if code, stop := parseFlags(fs, args); stop {
		return code
	}
	argv := fs.Args()
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "confobs: run needs a command to start, for example:\n\n  confobs run --schema schema.yaml -- python3 app.py")
		return exitUsage
	}

	l, err := load(c)
	if err != nil {
		return fail(stderr, err)
	}

	if l.result.HasProblems() {
		fmt.Fprintf(stderr, "confobs: not starting %q - %d config problem(s) against %s\n\n",
			argv[0], problemCount(l.result), l.schemaPath)
		writeProblems(stderr, l.result)
		return exitInvalid
	}

	if !quiet {
		writeWarnings(stderr, l.result)
	}

	if snapshotPath != "" {
		drift, err := l.schema.Drift(l.resolved.Values, snapshotPath, true)
		if err != nil {
			return fail(stderr, err)
		}
		if !quiet && drift.Changed() {
			fmt.Fprintf(stderr, "confobs: config drift since the last run (%s):\n", snapshotPath)
			writeChanges(stderr, drift.Changes)
		}
	}

	if !quiet {
		fmt.Fprintf(stderr, "confobs: config OK (%d fields, %s) - starting: %s\n",
			len(l.schema.Fields), l.schemaPath, strings.Join(argv, " "))
	}
	return runChild(argv, childEnvironment(l), stdout, stderr)
}

// childEnvironment is the real environment plus everything confobs resolved:
// schema defaults, and any env-file values that were not already set.
func childEnvironment(l *loaded) []string {
	effective := make(map[string]string, len(l.env))
	for k, v := range l.env {
		effective[k] = v
	}
	for k, v := range l.resolved.Values {
		effective[k] = v
	}

	var extra []string
	for k, v := range effective {
		if cur, ok := l.processEnv[k]; !ok || cur != v {
			extra = append(extra, k+"="+v)
		}
	}
	sort.Strings(extra) // deterministic

	// When a key appears twice in Cmd.Env, the last one wins, so appending is
	// enough to override.
	return append(os.Environ(), extra...)
}

func runChild(argv, env []string, stdout, stderr io.Writer) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "confobs: cannot start %q: %v\n", argv[0], err)
		return 127
	}

	// Forward termination and reload signals so that stopping or signalling the
	// wrapper behaves as if it had been the program itself (docker stop, systemd,
	// a manual kill -HUP). SIGINT is deliberately not forwarded: when it comes
	// from a terminal (Ctrl-C) the child already receives it directly as part of
	// the same foreground process group, and a second copy can abort a graceful
	// shutdown. We still catch it so the wrapper outlives the child and can
	// report its exit status.
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				if s != syscall.SIGINT {
					_ = cmd.Process.Signal(s)
				}
			case <-done:
				return
			}
		}
	}()

	err := cmd.Wait()
	signal.Stop(sigs)
	close(done)
	return exitStatus(err)
}

// exitStatus turns Wait's result into a shell-style exit code: the child's own
// code, or 128+N if it was killed by signal N.
func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 1
}
