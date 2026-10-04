package config

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

type CustomItem struct {
	Path      string  `json:"path"`                // Relative path under $HOME, e.g. ".bashrc", "scripts", "my_notes/todo.txt"
	Type      string  `json:"type,omitempty"`      // "file", "dir", or "auto"
	Deleted   bool    `json:"deleted,omitempty"`   // Tombstone for distributed deletion
	UpdatedAt float64 `json:"updated_at,omitempty"`
}

type CustomSyncRegistry map[string]*CustomItem

// NormalizeCustomPath validates that rawPath is within $HOME, expands tilde and relative paths,
// and returns the clean relative path to $HOME and whether it exists locally as a directory.
func NormalizeCustomPath(rawPath string) (relPath string, isDir bool, err error) {
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return "", false, fmt.Errorf("path cannot be empty")
	}

	// 1. Expand ~ or ~/
	resolved := rawPath
	if resolved == "~" {
		return "", false, fmt.Errorf("cannot add entire home directory directly. Please specify a specific file or directory under $HOME")
	}
	if strings.HasPrefix(resolved, "~/") {
		resolved = filepath.Join(HomeDir, resolved[2:])
	} else if strings.HasPrefix(resolved, "~") {
		resolved = filepath.Join(HomeDir, resolved[1:])
	}

	// 2. Resolve to absolute path
	var absPath string
	if filepath.IsAbs(resolved) {
		absPath = filepath.Clean(resolved)
	} else {
		// If relative, resolve against current working directory first
		cwd, err := os.Getwd()
		if err == nil {
			absPath = filepath.Clean(filepath.Join(cwd, resolved))
		} else {
			absPath = filepath.Clean(filepath.Join(HomeDir, resolved))
		}
	}

	cleanHome := filepath.Clean(HomeDir)

	// 3. Validation: Must be strictly under HomeDir
	if absPath == cleanHome {
		return "", false, fmt.Errorf("cannot add entire home directory directly. Please specify a specific file or directory under $HOME")
	}

	homePrefix := cleanHome + string(filepath.Separator)
	if !strings.HasPrefix(absPath, homePrefix) {
		return "", false, fmt.Errorf("path '%s' is outside $HOME (~/). Only files and directories under $HOME are supported for cross-platform compatibility", rawPath)
	}

	// 4. Extract relative path
	rel := strings.TrimPrefix(absPath, homePrefix)
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || rel == "" || strings.HasPrefix(rel, "..") {
		return "", false, fmt.Errorf("invalid path '%s'", rawPath)
	}

	// 5. Reserved / Built-in checks
	lowerRel := strings.ToLower(rel)
	if lowerRel == ".gemini" || strings.HasPrefix(lowerRel, ".gemini/") {
		return "", false, fmt.Errorf("path '~/%s' is an internal agy-sync system path and is already handled automatically", rel)
	}
	if lowerRel == "gemini.md" {
		return "", false, fmt.Errorf("path '~/%s' is already synchronized as constitutional rules", rel)
	}
	if lowerRel == ".ssh/authorized_keys" {
		return "", false, fmt.Errorf("path '~/%s' is already synchronized as cluster SSH authorized keys", rel)
	}
	if lowerRel == "agy-pool" || strings.HasPrefix(lowerRel, "agy-pool/") {
		return "", false, fmt.Errorf("path '~/%s' is already synchronized as account pool engine", rel)
	}
	if lowerRel == "agy-sync" || strings.HasPrefix(lowerRel, "agy-sync/") {
		return "", false, fmt.Errorf("path '~/%s' is already synchronized as synchronizer engine source", rel)
	}

	// 6. Check local existence and file type
	if fi, err := os.Stat(absPath); err == nil {
		isDir = fi.IsDir()
	} else {
		isDir = false
	}

	return rel, isDir, nil
}

// LoadCustomSyncRegistry loads custom_sync.json into memory.
func LoadCustomSyncRegistry() CustomSyncRegistry {
	reg := make(CustomSyncRegistry)
	data, err := os.ReadFile(CustomSyncFile)
	if err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &reg)
	}
	return reg
}

// SaveCustomSyncRegistry atomically persists the custom sync registry to disk.
func SaveCustomSyncRegistry(reg CustomSyncRegistry) error {
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return WriteFileAtomic(CustomSyncFile, data, 0644)
}

// MergeCustomSyncRegistries performs LWW (Last-Write-Wins) reconciliation between two nodes' custom registries.
func MergeCustomSyncRegistries(local, remote CustomSyncRegistry, clockSkew float64) (CustomSyncRegistry, bool, bool) {
	merged := make(CustomSyncRegistry)
	localChanged := false
	remoteChanged := false

	allKeys := make(map[string]bool)
	for k := range local {
		allKeys[k] = true
	}
	for k := range remote {
		allKeys[k] = true
	}

	for k := range allKeys {
		locItem, inLoc := local[k]
		remItem, inRem := remote[k]

		if inLoc && !inRem {
			merged[k] = locItem
			remoteChanged = true
		} else if inRem && !inLoc {
			merged[k] = remItem
			localChanged = true
		} else {
			// In both: compare normalized timestamps
			normRemTime := remItem.UpdatedAt - clockSkew
			tol := math.Max(0.5, math.Abs(clockSkew)+0.5)

			if locItem.UpdatedAt > normRemTime+tol {
				merged[k] = locItem
				remoteChanged = true
			} else if normRemTime > locItem.UpdatedAt+tol {
				merged[k] = remItem
				localChanged = true
			} else {
				// Within clock drift tolerance: latest timestamp wins, or deletion takes precedence
				if locItem.Deleted != remItem.Deleted {
					if locItem.UpdatedAt >= normRemTime {
						merged[k] = locItem
						if locItem.Deleted {
							remoteChanged = true
						} else {
							localChanged = true
						}
					} else {
						merged[k] = remItem
						if remItem.Deleted {
							localChanged = true
						} else {
							remoteChanged = true
						}
					}
				} else {
					merged[k] = locItem
				}
			}
		}
	}

	return merged, localChanged, remoteChanged
}
