package linux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestCommandFailureLogsExitCodeWithoutOutputOrArguments(t *testing.T) {
	if os.Getenv("CSI_COMMAND_LOG_CHILD") == "1" {
		fmt.Fprint(os.Stdout, "secret-command-output")
		fmt.Fprint(os.Stderr, "secret-command-stderr")
		os.Exit(13)
	}
	t.Setenv("CSI_COMMAND_LOG_CHILD", "1")
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	_, err := command(context.Background(), os.Args[0], "-test.run=^TestCommandFailureLogsExitCodeWithoutOutputOrArguments$", "--", "secret-command-argument")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 13 {
		t.Fatalf("lost exit cause: %v", err)
	}
	if strings.Contains(output.String(), "secret-command") || !strings.Contains(output.String(), `"exit_code":13`) || !strings.Contains(output.String(), `"duration_ms":`) {
		t.Fatalf("unsafe/incomplete command log: %s", output.String())
	}
}
