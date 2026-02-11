package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type ExecTool struct {
	workingDir          string
	timeout             time.Duration
	denyPatterns        []*regexp.Regexp
	allowPatterns       []*regexp.Regexp
	restrictToWorkspace bool
}

func NewExecTool(workingDir string) *ExecTool {
	// Minimal denylist — only catastrophic commands (OpenClaw-level trust)
	denyPatterns := []*regexp.Regexp{
		regexp.MustCompile(`\brm\s+-[rf]{2}\s+/\s*$`),         // rm -rf /
		regexp.MustCompile(`\brm\s+.*--no-preserve-root\b`),    // rm --no-preserve-root
		regexp.MustCompile(`\b(mkfs|format|diskpart)\b\s`),     // disk formatting
		regexp.MustCompile(`\bdd\s+.*of=/dev/sd[a-z]\b`),       // dd to disk
		regexp.MustCompile(`>\s*/dev/sd[a-z]\b`),               // redirect to disk
		regexp.MustCompile(`\b(shutdown|reboot|poweroff)\b`),   // system shutdown
		regexp.MustCompile(`:\(\)\s*\{.*\};\s*:`),              // fork bomb
	}

	return &ExecTool{
		workingDir:          workingDir,
		timeout:             60 * time.Second,
		denyPatterns:        denyPatterns,
		allowPatterns:       nil,
		restrictToWorkspace: false,
	}
}

func (t *ExecTool) Name() string {
	return "exec"
}

func (t *ExecTool) Description() string {
	return "Execute a shell command and return its output. Use with caution."
}

func (t *ExecTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"command": map[string]interface{}{
				"type":        "string",
				"description": "The shell command to execute",
			},
			"working_dir": map[string]interface{}{
				"type":        "string",
				"description": "Optional working directory for the command",
			},
		},
		"required": []string{"command"},
	}
}

func (t *ExecTool) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	command, ok := args["command"].(string)
	if !ok {
		return "", fmt.Errorf("command is required")
	}

	cwd := t.workingDir
	if wd, ok := args["working_dir"].(string); ok && wd != "" {
		// Validate that the requested working_dir is within the workspace
		if t.restrictToWorkspace && t.workingDir != "" {
			absWD, err := filepath.Abs(wd)
			if err != nil {
				return "Error: invalid working directory path", nil
			}
			absWorkspace, err := filepath.Abs(t.workingDir)
			if err != nil {
				return "Error: invalid workspace path", nil
			}
			// Ensure the requested dir is the workspace or a subdirectory of it
			if absWD != absWorkspace && !strings.HasPrefix(absWD, absWorkspace+string(filepath.Separator)) {
				return "Error: working_dir must be within the workspace", nil
			}
		}
		cwd = wd
	}

	if cwd == "" {
		wd, err := os.Getwd()
		if err == nil {
			cwd = wd
		}
	}

	if guardError := t.guardCommand(command, cwd); guardError != "" {
		return fmt.Sprintf("Error: %s", guardError), nil
	}

	cmdCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", command)
	if cwd != "" {
		cmd.Dir = cwd
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\nSTDERR:\n" + stderr.String()
	}

	if err != nil {
		if cmdCtx.Err() == context.DeadlineExceeded {
			return fmt.Sprintf("Error: Command timed out after %v", t.timeout), nil
		}
		output += fmt.Sprintf("\nExit code: %v", err)
	}

	if output == "" {
		output = "(no output)"
	}

	maxLen := 10000
	if len(output) > maxLen {
		output = output[:maxLen] + fmt.Sprintf("\n... (truncated, %d more chars)", len(output)-maxLen)
	}

	return output, nil
}

func (t *ExecTool) guardCommand(command, cwd string) string {
	cmd := strings.TrimSpace(command)
	lower := strings.ToLower(cmd)

	for _, pattern := range t.denyPatterns {
		if pattern.MatchString(lower) {
			return "Command blocked by safety guard (dangerous pattern detected)"
		}
	}

	if len(t.allowPatterns) > 0 {
		allowed := false
		for _, pattern := range t.allowPatterns {
			if pattern.MatchString(lower) {
				allowed = true
				break
			}
		}
		if !allowed {
			return "Command blocked by safety guard (not in allowlist)"
		}
	}

	if t.restrictToWorkspace {
		if strings.Contains(cmd, "..\\") || strings.Contains(cmd, "../") {
			return "Command blocked by safety guard (path traversal detected)"
		}

		// Use the workspace root (not the current cwd) as the boundary
		workspacePath := t.workingDir
		if workspacePath == "" {
			workspacePath = cwd
		}
		absWorkspace, err := filepath.Abs(workspacePath)
		if err != nil {
			return "Command blocked by safety guard (cannot resolve workspace path)"
		}

		pathPattern := regexp.MustCompile(`[A-Za-z]:\\[^\\\"']+|/[^\s\"']+`)
		matches := pathPattern.FindAllString(cmd, -1)

		for _, raw := range matches {
			// Allow common safe system paths that commands legitimately reference
			if raw == "/dev/null" || raw == "/dev/stdin" || raw == "/dev/stdout" || raw == "/dev/stderr" ||
				raw == "/tmp" || strings.HasPrefix(raw, "/tmp/") {
				continue
			}
			p, err := filepath.Abs(raw)
			if err != nil {
				continue
			}
			// Path must be within the workspace
			if p != absWorkspace && !strings.HasPrefix(p, absWorkspace+string(filepath.Separator)) {
				return "Command blocked by safety guard (path outside workspace)"
			}
		}
	}

	return ""
}

func (t *ExecTool) SetTimeout(timeout time.Duration) {
	t.timeout = timeout
}

func (t *ExecTool) SetRestrictToWorkspace(restrict bool) {
	t.restrictToWorkspace = restrict
}

func (t *ExecTool) SetAllowPatterns(patterns []string) error {
	t.allowPatterns = make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("invalid allow pattern %q: %w", p, err)
		}
		t.allowPatterns = append(t.allowPatterns, re)
	}
	return nil
}
