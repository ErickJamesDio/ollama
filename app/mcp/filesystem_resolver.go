package mcp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const filesystemServerPackage = "@modelcontextprotocol/server-filesystem"

type filesystemPackageJSON struct {
	Name string          `json:"name"`
	Bin  json.RawMessage `json:"bin"`
}

func resolveInstalledFilesystemServer(cfg Config) (Config, bool) {
	serverArgs, ok := filesystemServerArgs(cfg)
	if !ok {
		return cfg, false
	}

	node, err := exec.LookPath("node")
	if err != nil {
		return cfg, false
	}

	entrypoint := newestFilesystemEntrypoint(filesystemSearchRoots())
	if entrypoint == "" {
		return cfg, false
	}

	resolved := cfg
	resolved.Command = node
	resolved.Args = append([]string{entrypoint}, serverArgs...)
	return resolved, true
}

func filesystemServerArgs(cfg Config) ([]string, bool) {
	command := strings.ToLower(filepath.Base(strings.TrimSpace(cfg.Command)))
	args := cfg.Args
	if command == "cmd" || command == "cmd.exe" {
		if len(args) < 2 || !strings.EqualFold(args[0], "/c") {
			return nil, false
		}
		command = strings.ToLower(filepath.Base(args[1]))
		args = args[2:]
	}
	if command != "npx" && command != "npx.cmd" && command != "npx.exe" {
		return nil, false
	}
	for index, arg := range args {
		if arg == filesystemServerPackage {
			return append([]string(nil), args[index+1:]...), true
		}
	}
	return nil, false
}

func filesystemSearchRoots() []string {
	var roots []string
	if appData := os.Getenv("APPDATA"); appData != "" {
		roots = append(roots, filepath.Join(appData, "npm", "node_modules"))
	}
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		roots = append(roots, filepath.Join(localAppData, "npm-cache", "_npx"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, ".npm", "_npx"))
	}
	if runtime.GOOS != "windows" {
		roots = append(roots, "/usr/local/lib/node_modules", "/usr/lib/node_modules")
	}
	return roots
}

func newestFilesystemEntrypoint(roots []string) string {
	var newestPath string
	var newestTime time.Time
	for _, root := range roots {
		packageDirs := []string{filepath.Join(root, "@modelcontextprotocol", "server-filesystem")}
		if filepath.Base(root) == "_npx" {
			entries, err := os.ReadDir(root)
			if err != nil {
				continue
			}
			packageDirs = packageDirs[:0]
			for _, entry := range entries {
				if entry.IsDir() {
					packageDirs = append(packageDirs, filepath.Join(root, entry.Name(), "node_modules", "@modelcontextprotocol", "server-filesystem"))
				}
			}
		}

		for _, packageDir := range packageDirs {
			entrypoint, modified, ok := filesystemEntrypoint(packageDir)
			if ok && (newestPath == "" || modified.After(newestTime)) {
				newestPath = entrypoint
				newestTime = modified
			}
		}
	}
	return newestPath
}

func filesystemEntrypoint(packageDir string) (string, time.Time, bool) {
	manifestPath := filepath.Join(packageDir, "package.json")
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", time.Time{}, false
	}
	var manifest filesystemPackageJSON
	if json.Unmarshal(body, &manifest) != nil || manifest.Name != filesystemServerPackage {
		return "", time.Time{}, false
	}

	var bins map[string]string
	if json.Unmarshal(manifest.Bin, &bins) != nil {
		return "", time.Time{}, false
	}
	relative := bins["mcp-server-filesystem"]
	if relative == "" || filepath.IsAbs(relative) {
		return "", time.Time{}, false
	}
	entrypoint := filepath.Clean(filepath.Join(packageDir, relative))
	rel, err := filepath.Rel(packageDir, entrypoint)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", time.Time{}, false
	}
	info, err := os.Stat(entrypoint)
	if err != nil || info.IsDir() {
		return "", time.Time{}, false
	}
	manifestInfo, err := os.Stat(manifestPath)
	if err != nil {
		return "", time.Time{}, false
	}
	return entrypoint, manifestInfo.ModTime(), true
}
