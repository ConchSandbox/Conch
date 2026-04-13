package image

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExtensionPlan captures Conch Dockerfile extensions before vanilla buildah runs.
type ExtensionPlan struct {
	KernelFile string
	InitrdFile string
	NeedIndex  bool
	NeedSnap   bool
}

// PreprocessResult is the output of PreprocessDockerfile (see docs/CONCH_BUILD_API.md).
type PreprocessResult struct {
	TempDockerfile string
	Plan           ExtensionPlan
}

// PreprocessDockerfile reads the Dockerfile, validates Conch extension usage, optionally checks
// KERNEL paths under contextDir, and writes a temporary Dockerfile without extension lines.
func PreprocessDockerfile(dockerfilePath, contextDir string) (PreprocessResult, error) {
	var res PreprocessResult
	var absCtx string
	if contextDir != "" {
		var err error
		absCtx, err = filepath.Abs(contextDir)
		if err != nil {
			return res, fmt.Errorf("context directory: %w", err)
		}
	}

	f, err := os.Open(dockerfilePath)
	if err != nil {
		return res, fmt.Errorf("open dockerfile: %w", err)
	}
	defer f.Close()

	var out strings.Builder
	sc := bufio.NewScanner(f)
	lineNo := 0
	kernelCount, indexCount, snapCount := 0, 0, 0

	for sc.Scan() {
		lineNo++
		line := sc.Text()
		trim := strings.TrimSpace(line)
		fields := strings.Fields(trim)

		if len(fields) > 0 && strings.EqualFold(fields[0], "KERNEL") {
			kernelCount++
			if len(fields) != 3 {
				return res, fmt.Errorf("dockerfile line %d: KERNEL requires exactly two arguments (kernel_file initrd_file)", lineNo)
			}
			res.Plan.KernelFile = fields[1]
			res.Plan.InitrdFile = fields[2]
			continue
		}
		if len(fields) == 1 && strings.EqualFold(fields[0], "SNAP") {
			if kernelCount == 0 {
				return res, fmt.Errorf("dockerfile line %d: SNAP requires a preceding KERNEL <kernel> <initrd>", lineNo)
			}
			snapCount++
			res.Plan.NeedSnap = true
			continue
		}
		if len(fields) == 1 && strings.EqualFold(fields[0], "INDEX") {
			if kernelCount == 0 {
				return res, fmt.Errorf("dockerfile line %d: INDEX requires a preceding KERNEL <kernel> <initrd>", lineNo)
			}
			indexCount++
			res.Plan.NeedIndex = true
			continue
		}

		out.WriteString(line)
		out.WriteString("\n")
	}
	if err := sc.Err(); err != nil {
		return res, fmt.Errorf("read dockerfile: %w", err)
	}

	if snapCount > 1 {
		return res, fmt.Errorf("dockerfile: at most one SNAP instruction is allowed")
	}
	if indexCount > 1 {
		return res, fmt.Errorf("dockerfile: at most one INDEX instruction is allowed")
	}
	if res.Plan.NeedSnap && kernelCount == 0 {
		return res, fmt.Errorf("dockerfile: SNAP requires a preceding KERNEL <kernel> <initrd>")
	}
	if res.Plan.NeedIndex && kernelCount == 0 {
		return res, fmt.Errorf("dockerfile: INDEX requires a preceding KERNEL <kernel> <initrd>")
	}
	if res.Plan.NeedIndex && res.Plan.NeedSnap {
		return res, fmt.Errorf("dockerfile: INDEX and SNAP cannot be used together")
	}
	if kernelCount > 1 {
		return res, fmt.Errorf("dockerfile: at most one KERNEL instruction is allowed in this version")
	}

	if absCtx != "" && res.Plan.KernelFile != "" && res.Plan.InitrdFile != "" {
		for _, name := range []string{res.Plan.KernelFile, res.Plan.InitrdFile} {
			p := filepath.Clean(filepath.Join(absCtx, name))
			rel, relErr := filepath.Rel(absCtx, p)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				return res, fmt.Errorf("KERNEL file %q escapes context directory", name)
			}
			if st, stErr := os.Stat(p); stErr != nil {
				return res, fmt.Errorf("KERNEL file %q under context: %w", name, stErr)
			} else if st.IsDir() {
				return res, fmt.Errorf("KERNEL path %q is a directory", name)
			}
		}
	}

	tmp, err := os.CreateTemp(filepath.Dir(dockerfilePath), "conch-dockerfile-*.tmp")
	if err != nil {
		return res, fmt.Errorf("create temp dockerfile: %w", err)
	}
	path := tmp.Name()
	if _, err := tmp.WriteString(out.String()); err != nil {
		tmp.Close()
		os.Remove(path)
		return res, fmt.Errorf("write temp dockerfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(path)
		return res, err
	}
	res.TempDockerfile = path
	return res, nil
}
