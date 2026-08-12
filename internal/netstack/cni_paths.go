package netstack

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/pkg/ulog"
)

// The search path decides which binary conchd executes as root. The paths come
// from the config file, which is trusted input; their permissions on disk do
// not, and a search directory another local user can write to hands that user
// the daemon's privileges. These checks reject that, and only that.

// What libcni loads from a plugin conf dir.
var cniConfigFileExtensions = []string{".conf", ".conflist", ".json"}

// validateCNIPluginPaths checks every search directory and the files in them.
func validateCNIPluginPaths(cfg CNIManagerConfig) error {
	for _, dir := range cfg.PluginBinDirs {
		if err := validateCNISearchDir(dir, "network.cni.plugin_bin_dirs", nil); err != nil {
			return err
		}
	}
	return validateCNISearchDir(cfg.PluginConfDir, "network.cni.plugin_conf_dir", cniConfigFileExtensions)
}

// validateCNISearchDir checks the ancestor chain, the directory, and its files.
// fileExtensions nil means every regular file is a candidate, which is the case
// for a bin dir since any file there can be selected by name from a config.
func validateCNISearchDir(dir, field string, fileExtensions []string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf(
			"%s: %q is not an absolute path; a relative search path resolves against "+
				"the daemon's working directory, which is not part of the configuration",
			field, dir,
		)
	}

	for _, ancestor := range ancestorsOf(dir) {
		if err := checkCNIPathComponent(ancestor, field, dir, componentAncestor); err != nil {
			return err
		}
	}

	var stat unix.Stat_t
	if err := unix.Stat(dir, &stat); err != nil {
		if os.IsNotExist(err) {
			// go-cni tolerates missing dirs. The ancestors were still checked,
			// which is what stops someone else creating this one later.
			ulog.GetLogger().Warn("CNI search directory does not exist",
				ulog.F("field", field),
				ulog.F("dir", dir),
			)
			return nil
		}
		return fmt.Errorf("%s: failed to inspect %q: %w", field, dir, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("%s: %q is not a directory", field, dir)
	}
	if err := checkCNIPathComponent(dir, field, dir, componentSearchDir); err != nil {
		return err
	}

	return checkCNIDirEntries(dir, field, fileExtensions)
}

type componentKind int

const (
	// Writing to an ancestor means renaming the child away, which is exactly
	// what sticky prevents, so sticky exempts it.
	componentAncestor componentKind = iota
	// Sticky does not exempt the search dir: the attack is creating a plugin
	// that does not exist yet, which sticky does not restrict.
	componentSearchDir
	componentFile
)

func (k componentKind) describe() string {
	switch k {
	case componentAncestor:
		return "parent directory"
	case componentSearchDir:
		return "search directory"
	default:
		return "file"
	}
}

// Rejects a component another user can modify; warns on a group grant.
func checkCNIPathComponent(path, field, dir string, kind componentKind) error {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("%s: failed to inspect %q: %w", field, path, err)
	}

	// An owner can chmod at will, whatever the mode currently says.
	if daemonUID := uint32(os.Getuid()); stat.Uid != 0 && stat.Uid != daemonUID {
		return fmt.Errorf(
			"%s: %s %q%s is owned by uid %d, which is neither root nor the daemon (uid %d); "+
				"that user can replace the CNI plugins conch executes",
			field, kind.describe(), path, resolvedNote(path), stat.Uid, daemonUID,
		)
	}

	sticky := stat.Mode&unix.S_ISVTX != 0
	if stat.Mode&0o002 != 0 && !(kind == componentAncestor && sticky) {
		return fmt.Errorf(
			"%s: %s %q%s is world-writable (mode %04o); any local user can supply the "+
				"CNI plugins conch executes as root",
			field, kind.describe(), path, resolvedNote(path), stat.Mode&0o7777,
		)
	}
	if stat.Mode&0o020 != 0 && !(kind == componentAncestor && sticky) {
		// Warn, not reject: a group grant can be a deliberate boundary.
		ulog.GetLogger().Warn("CNI path is group-writable",
			ulog.F("field", field),
			ulog.F("dir", dir),
			ulog.F("path", path),
			ulog.F("mode", fmt.Sprintf("%04o", stat.Mode&0o7777)),
			ulog.F("gid", stat.Gid),
		)
	}
	return nil
}

// Names where a path really leads, so the refusal does not point at a directory
// whose own mode looks fine.
func resolvedNote(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved == path {
		return ""
	}
	return fmt.Sprintf(" (resolves to %q)", resolved)
}

// A private directory says nothing about a world-writable file inside it, and
// overwriting such a plugin in place is the whole of the reported attack.
func checkCNIDirEntries(dir, field string, fileExtensions []string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		ulog.GetLogger().Warn("Failed to list CNI search directory",
			ulog.F("field", field),
			ulog.F("dir", dir),
			ulog.F("error", err),
		)
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() || !relevantCNIFile(entry.Name(), fileExtensions) {
			continue
		}
		if err := checkCNIPathComponent(filepath.Join(dir, entry.Name()), field, dir, componentFile); err != nil {
			return err
		}
	}
	return nil
}

func relevantCNIFile(name string, fileExtensions []string) bool {
	if fileExtensions == nil {
		return true
	}
	ext := filepath.Ext(name)
	for _, want := range fileExtensions {
		if ext == want {
			return true
		}
	}
	return false
}

// Both chains are needed: resolving alone misses the directory a symlink lives
// in, whose writers can repoint it; not resolving misses the real directories
// traversed.
func ancestorsOf(dir string) []string {
	chains := [][]string{pathChain(dir)}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != dir {
		chains = append(chains, pathChain(resolved))
	}

	seen := make(map[string]struct{})
	var ancestors []string
	for _, chain := range chains {
		// The last element is the directory itself, checked more strictly.
		for _, component := range chain[:len(chain)-1] {
			if _, ok := seen[component]; ok {
				continue
			}
			seen[component] = struct{}{}
			ancestors = append(ancestors, component)
		}
	}
	return ancestors
}

// pathChain returns "/a/b/c" as ["/", "/a", "/a/b", "/a/b/c"].
func pathChain(dir string) []string {
	cleaned := filepath.Clean(dir)
	chain := []string{"/"}
	current := "/"
	for _, part := range strings.Split(strings.TrimPrefix(cleaned, "/"), "/") {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		chain = append(chain, current)
	}
	return chain
}
