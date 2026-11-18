package convert

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ToolError 外部命令执行失败的详细信息，便于在任务列表里定位问题。
type ToolError struct {
	Tool   string
	Args   []string
	Stderr string
	Err    error
}

func (e *ToolError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if len(msg) > 800 {
		msg = msg[:800] + "..."
	}
	if msg == "" {
		return fmt.Sprintf("%s 执行失败: %v", e.Tool, e.Err)
	}
	return fmt.Sprintf("%s 执行失败: %v (%s)", e.Tool, e.Err, msg)
}

func (e *ToolError) Unwrap() error { return e.Err }

// run 执行外部命令，带超时与 stdout/stderr 捕获。
func run(ctx context.Context, timeout time.Duration, tool string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, tool, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		return stdout.String(), &ToolError{Tool: tool, Args: args, Stderr: "执行超时", Err: cctx.Err()}
	}
	if err != nil {
		return stdout.String(), &ToolError{Tool: tool, Args: args, Stderr: stderr.String(), Err: err}
	}
	return stdout.String(), nil
}
