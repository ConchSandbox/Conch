package netstack

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func binDirConfig(dirs ...string) CNIManagerConfig {
	return CNIManagerConfig{PluginBinDirs: dirs, PluginConfDir: confDirPlaceholder}
}

// Always passes, so a bin-dir test fails only for its own reason.
const confDirPlaceholder = "/usr/libexec/conch-cni-absent"

func mkdir(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(path, mode); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	// MkdirAll applies the ambient umask.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	return path
}

func writeFile(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	return path
}

func TestValidateCNIPluginPathsAcceptsPrivateDir(t *testing.T) {
	dir := mkdir(t, filepath.Join(t.TempDir(), "cni"), 0o755)
	writeFile(t, filepath.Join(dir, "bridge"), 0o755)

	if err := validateCNIPluginPaths(binDirConfig(dir)); err != nil {
		t.Fatalf("validateCNIPluginPaths: %v", err)
	}
}

func TestValidateCNIPluginPathsRejectsWorldWritableSearchDir(t *testing.T) {
	dir := mkdir(t, filepath.Join(t.TempDir(), "cni"), 0o777)

	err := validateCNIPluginPaths(binDirConfig(dir))
	if err == nil {
		t.Fatal("a world-writable search directory was accepted")
	}
	if !strings.Contains(err.Error(), "world-writable") || !strings.Contains(err.Error(), dir) {
		t.Errorf("error does not name the problem or the path: %v", err)
	}
}

// Sticky looks like it should make a shared directory safe, and for a parent it
// does. Not here: the attack is creating a file that does not exist yet.
func TestValidateCNIPluginPathsRejectsStickySearchDir(t *testing.T) {
	dir := mkdir(t, filepath.Join(t.TempDir(), "cni"), 0o777|os.ModeSticky)

	if err := validateCNIPluginPaths(binDirConfig(dir)); err == nil {
		t.Fatal("a sticky world-writable search directory was accepted")
	}
}

func TestValidateCNIPluginPathsRejectsWorldWritableAncestor(t *testing.T) {
	parent := mkdir(t, filepath.Join(t.TempDir(), "shared"), 0o777)
	dir := mkdir(t, filepath.Join(parent, "cni"), 0o755)

	err := validateCNIPluginPaths(binDirConfig(dir))
	if err == nil {
		t.Fatal("a world-writable parent was accepted; the directory can be renamed away")
	}
	if !strings.Contains(err.Error(), parent) {
		t.Errorf("error does not name the offending parent: %v", err)
	}
}

// The other half: /tmp is 1777, and a root-owned directory inside it cannot be
// renamed away, so a sticky parent is not a finding.
func TestValidateCNIPluginPathsAllowsStickyAncestor(t *testing.T) {
	parent := mkdir(t, filepath.Join(t.TempDir(), "shared"), 0o777|os.ModeSticky)
	dir := mkdir(t, filepath.Join(parent, "cni"), 0o755)

	if err := validateCNIPluginPaths(binDirConfig(dir)); err != nil {
		t.Fatalf("a sticky parent was rejected: %v", err)
	}
}

// What a directory-only check misses: the directory is private but the plugin
// inside it can be overwritten in place.
func TestValidateCNIPluginPathsRejectsWorldWritablePluginFile(t *testing.T) {
	dir := mkdir(t, filepath.Join(t.TempDir(), "cni"), 0o755)
	plugin := writeFile(t, filepath.Join(dir, "bridge"), 0o777)

	err := validateCNIPluginPaths(binDirConfig(dir))
	if err == nil {
		t.Fatal("a world-writable plugin binary was accepted")
	}
	if !strings.Contains(err.Error(), plugin) {
		t.Errorf("error does not name the offending file: %v", err)
	}
}

func TestValidateCNIPluginPathsRejectsRelativePath(t *testing.T) {
	for _, dir := range []string{"cni", "./cni", "../cni"} {
		t.Run(dir, func(t *testing.T) {
			err := validateCNIPluginPaths(binDirConfig(dir))
			if err == nil {
				t.Fatalf("relative path %q was accepted", dir)
			}
			if !strings.Contains(err.Error(), "absolute") {
				t.Errorf("error does not explain the refusal: %v", err)
			}
		})
	}
}

func TestValidateCNIPluginPathsRejectsNonDirectory(t *testing.T) {
	file := writeFile(t, filepath.Join(t.TempDir(), "cni"), 0o755)

	err := validateCNIPluginPaths(binDirConfig(file))
	if err == nil {
		t.Fatal("a regular file was accepted as a search directory")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error does not explain the refusal: %v", err)
	}
}

// go-cni tolerates missing dirs; the ancestors were still validated.
func TestValidateCNIPluginPathsAllowsMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")

	if err := validateCNIPluginPaths(binDirConfig(missing)); err != nil {
		t.Fatalf("a missing search directory was rejected: %v", err)
	}
}

func TestValidateCNIPluginPathsRejectsMissingDirUnderWritableParent(t *testing.T) {
	parent := mkdir(t, filepath.Join(t.TempDir(), "shared"), 0o777)

	if err := validateCNIPluginPaths(binDirConfig(filepath.Join(parent, "absent"))); err == nil {
		t.Fatal("a missing directory under a world-writable parent was accepted")
	}
}

func TestValidateCNIPluginPathsChecksConfDir(t *testing.T) {
	binDir := mkdir(t, filepath.Join(t.TempDir(), "cni"), 0o755)
	confDir := mkdir(t, filepath.Join(t.TempDir(), "net.d"), 0o777)

	err := validateCNIPluginPaths(CNIManagerConfig{
		PluginBinDirs: []string{binDir},
		PluginConfDir: confDir,
	})
	if err == nil {
		t.Fatal("a world-writable conf directory was accepted; it decides which plugin runs")
	}
	if !strings.Contains(err.Error(), "plugin_conf_dir") {
		t.Errorf("error does not name the field: %v", err)
	}
}

// Only files libcni loads matter, so an unrelated world-writable file is not a
// finding while a .conflist is.
func TestValidateCNIPluginPathsScopesConfDirFileCheck(t *testing.T) {
	binDir := mkdir(t, filepath.Join(t.TempDir(), "cni"), 0o755)
	confDir := mkdir(t, filepath.Join(t.TempDir(), "net.d"), 0o755)
	writeFile(t, filepath.Join(confDir, "NOTES.txt"), 0o666)

	cfg := CNIManagerConfig{PluginBinDirs: []string{binDir}, PluginConfDir: confDir}
	if err := validateCNIPluginPaths(cfg); err != nil {
		t.Fatalf("an unrelated world-writable file was treated as a CNI config: %v", err)
	}

	writeFile(t, filepath.Join(confDir, "10-conch.conflist"), 0o666)
	if err := validateCNIPluginPaths(cfg); err == nil {
		t.Fatal("a world-writable .conflist was accepted")
	}
}

// Resolved chain: the configured path looks private, the target is not.
func TestValidateCNIPluginPathsResolvesSymlinkedDir(t *testing.T) {
	base := t.TempDir()
	target := mkdir(t, filepath.Join(base, "target"), 0o777)
	link := filepath.Join(base, "cni")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	err := validateCNIPluginPaths(binDirConfig(link))
	if err == nil {
		t.Fatal("a symlink to a world-writable directory was accepted")
	}
	if !strings.Contains(err.Error(), target) {
		t.Errorf("error does not name the resolved target: %v", err)
	}
}

// Unresolved chain: the target is private but the symlink can be repointed.
func TestValidateCNIPluginPathsChecksSymlinkParent(t *testing.T) {
	base := t.TempDir()
	target := mkdir(t, filepath.Join(base, "target"), 0o755)
	linkParent := mkdir(t, filepath.Join(base, "shared"), 0o777)
	link := filepath.Join(linkParent, "cni")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	err := validateCNIPluginPaths(binDirConfig(link))
	if err == nil {
		t.Fatal("a symlink in a world-writable directory was accepted; anyone can repoint it")
	}
	if !strings.Contains(err.Error(), linkParent) {
		t.Errorf("error does not name the writable symlink parent: %v", err)
	}
}

// What a mode check alone misses: an owner can chmod at will.
func TestValidateCNIPluginPathsRejectsForeignOwner(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("must run as root to chown to another user")
	}
	dir := mkdir(t, filepath.Join(t.TempDir(), "cni"), 0o755)
	if err := os.Chown(dir, 65534, 65534); err != nil {
		t.Skipf("cannot chown to nobody: %v", err)
	}

	err := validateCNIPluginPaths(binDirConfig(dir))
	if err == nil {
		t.Fatal("a directory owned by another user was accepted")
	}
	if !strings.Contains(err.Error(), "owned by uid 65534") {
		t.Errorf("error does not name the owner: %v", err)
	}
}

func TestValidateCNIPluginPathsChecksEveryBinDir(t *testing.T) {
	good := mkdir(t, filepath.Join(t.TempDir(), "good"), 0o755)
	bad := mkdir(t, filepath.Join(t.TempDir(), "bad"), 0o777)

	if err := validateCNIPluginPaths(binDirConfig(good, bad)); err == nil {
		t.Fatal("only the first search directory was checked")
	}
}

func TestPathChain(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"/usr/libexec/cni", []string{"/", "/usr", "/usr/libexec", "/usr/libexec/cni"}},
		{"/opt", []string{"/", "/opt"}},
		{"/", []string{"/"}},
		{"/a//b/", []string{"/", "/a", "/a/b"}},
	}
	for _, tt := range tests {
		if got := pathChain(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("pathChain(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// Through the constructor: a correct helper wired in wrongly still executes.
func TestNewCNIManagerRejectsWorldWritablePluginDir(t *testing.T) {
	dir := mkdir(t, filepath.Join(t.TempDir(), "cni"), 0o777)

	_, err := NewCNIManager(CNIManagerConfig{PluginBinDirs: []string{dir}})
	if err == nil {
		t.Fatal("NewCNIManager accepted a world-writable plugin directory")
	}
	if !strings.Contains(err.Error(), "world-writable") {
		t.Errorf("NewCNIManager error = %v, want the world-writable refusal", err)
	}
}
