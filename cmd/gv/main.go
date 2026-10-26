package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
)

const defaultEndpoint = "https://api.grepvine.ai"

type app struct {
	stdout     io.Writer
	stderr     io.Writer
	stdin      io.Reader
	getenv     func(string) string
	configPath string
	httpClient *http.Client
	// isTTY reports whether stderr is a terminal, which decides whether a
	// progress line is drawn. stdoutTTY decides how search results are laid
	// out and whether they are colored.
	isTTY     bool
	stdoutTTY bool
	// status is the progress line of the command currently running, when one
	// is drawing. Messages that would otherwise scroll it away go through it.
	status   atomic.Pointer[progress]
	throttle throttle
	open     func(string) error
	sleep    func(context.Context, time.Duration) error
	exitCode int
	// uploadConcurrency is how many batches an upload keeps in flight; zero
	// means defaultUploadConcurrency. compression holds the upload's
	// --compress mode for the transport.
	uploadConcurrency int
	// jsonOutput is the --json flag of upload and sync: report each refused
	// document and, last, the totals as JSON lines on stdout.
	jsonOutput  bool
	totals      transferSummary
	compression atomic.Int32
	// openS3 returns the client an upload reads an S3 bucket through; nil
	// means the default AWS configuration chain.
	openS3 func(ctx context.Context, bucket string) (s3API, error)
}

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := &app{
		stdout: stdout, stderr: stderr, stdin: stdin, getenv: os.Getenv,
		open: openBrowser,
		sleep: func(ctx context.Context, duration time.Duration) error {
			timer := time.NewTimer(duration)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
	a.httpClient = newHTTPClient(&a.compression)
	a.isTTY = isTerminal(stderr)
	a.stdoutTTY = isTerminal(stdout)
	command := newRootCommand(a)
	command.SetArgs(args)
	if err := command.Execute(); err != nil {
		fmt.Fprintln(stderr, "gv:", err)
		if coded, ok := errors.AsType[*exitError](err); ok {
			return coded.code
		}
		return 2
	}
	return a.exitCode
}

func newRootCommand(a *app) *cobra.Command {
	root := &cobra.Command{
		Use:           "gv",
		Short:         "Search and manage documents and organizations with Grepvine",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.Version = versionString(debug.ReadBuildInfo())
	root.SetIn(a.stdin)
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.PersistentFlags().StringVar(&a.configPath, "config", a.configPath, "configuration file (default $GREPVINE_CONFIG, then ~/.grepvine/config.yaml)")
	root.AddCommand(
		newLoginCommand(a),
		newLogoutCommand(a),
		newSearchCommand(a),
		newUploadCommand(a),
		newSyncCommand(a),
		newGetDocumentCommand(a),
		newListDocumentsCommand(a),
		newDeleteDocumentCommand(a),
		newIndexCommand(a),
		newWhoamiCommand(a),
		newOrgCommand(a),
		newMemberCommand(a),
		newKeyCommand(a),
	)
	return root
}

// isTerminal reports whether w is an open character device such as a
// terminal, as opposed to a pipe, a file or an in-memory buffer.
func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func openBrowser(target string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name, args = "open", []string{target}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", target}
	default:
		name, args = "xdg-open", []string{target}
	}
	// The opener outlives the command, so it is not tied to its context.
	cmd := exec.CommandContext(context.Background(), name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
