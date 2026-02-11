package tools

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ValidatePath resolves the given path to an absolute path and checks that it
// falls within allowedDir. It returns the resolved absolute path or an error
// if the path escapes the workspace boundary.
func ValidatePath(path, allowedDir string) (string, error) {
	if allowedDir == "" {
		// No restriction configured — resolve and return.
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("failed to resolve path: %w", err)
		}
		return abs, nil
	}

	allowedAbs, err := filepath.Abs(allowedDir)
	if err != nil {
		return "", fmt.Errorf("failed to resolve allowed directory: %w", err)
	}
	// Ensure the allowed dir ends with separator for prefix matching so that
	// "/workspace-extra/foo" is not confused with "/workspace/foo".
	allowedPrefix := allowedAbs + string(filepath.Separator)

	var absPath string
	if filepath.IsAbs(path) {
		absPath = filepath.Clean(path)
	} else {
		absPath, err = filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("failed to resolve path: %w", err)
		}
	}

	// Allow the directory itself and anything beneath it.
	if absPath != allowedAbs && !strings.HasPrefix(absPath, allowedPrefix) {
		return "", fmt.Errorf("access denied: path %q is outside workspace %q", path, allowedDir)
	}

	return absPath, nil
}
