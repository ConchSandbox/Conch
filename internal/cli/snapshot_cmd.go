package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/image/client"
)

type snapshotExportOptions struct {
	configPath string
	namespace  string
	snapshotID string
	sandboxID  string
	tag        string
}

func printSnapshotImageHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch snapshot-image export [options]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Subcommands:")
	fmt.Fprintln(out, "  export  Export a sandbox-snapshot image from an existing rootfs snapshot or sandbox.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Common usage:")
	fmt.Fprintln(out, "  conch snapshot-image export --snapshot-id <rootfs-snapshot-id> -t <sandbox-snapshot-image>")
	fmt.Fprintln(out, "  conch snapshot-image export --sandbox-id <sandbox-id> -t <sandbox-snapshot-image>")
}

func printSnapshotsHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch snapshots ls [options]")
	fmt.Fprintln(out, "  conch snapshots rm [options] <snapshot-key>")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Subcommands:")
	fmt.Fprintln(out, "  ls  List EROFS snapshots from conchd/containerd.")
	fmt.Fprintln(out, "  rm  Remove an EROFS snapshot from conchd/containerd.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Common usage:")
	fmt.Fprintln(out, "  conch snapshots ls")
	fmt.Fprintln(out, "  conch snapshots rm <snapshot-key>")
	fmt.Fprintln(out, "  conch snapshots rm --cascade <rootfs-snapshot-key>")
}

func printSnapshotImageExportHelp(out io.Writer, fs *flag.FlagSet) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch snapshot-image export [options]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Description:")
	fmt.Fprintln(out, "  Export a Conch sandbox-snapshot image from an existing rootfs snapshot")
	fmt.Fprintln(out, "  or by pausing an existing sandbox.")
	fmt.Fprintln(out, "  Either one of --snapshot-id or --sandbox-id is required.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Options:")
	if fs != nil {
		fs.SetOutput(out)
		fs.PrintDefaults()
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Examples:")
	fmt.Fprintln(out, "  conch snapshot-image export --snapshot-id sha256:abc -t localhost/conch/sandbox-snapshot:latest")
	fmt.Fprintln(out, "  conch snapshot-image export --sandbox-id sandbox-123 -t localhost/conch/sandbox-snapshot:latest")
}

func newSnapshotImageExportFlagSet(out io.Writer) (*flag.FlagSet, *snapshotExportOptions) {
	fs := flag.NewFlagSet("snapshot-image export", flag.ContinueOnError)
	fs.SetOutput(out)

	var opts snapshotExportOptions
	fs.StringVar(&opts.snapshotID, "snapshot-id", "", "rootfs snapshot ID to export")
	fs.StringVar(&opts.sandboxID, "sandbox-id", "", "sandbox ID to pause and export")
	fs.StringVar(&opts.tag, "t", "", "output sandbox-snapshot image tag")
	fs.StringVar(&opts.tag, "tag", "", "output sandbox-snapshot image tag")
	fs.StringVar(&opts.namespace, "namespace", "", "containerd namespace")
	fs.StringVar(&opts.namespace, "n", "", "containerd namespace")
	fs.StringVar(&opts.configPath, "config", "", "config file path")
	fs.Usage = func() { printSnapshotImageExportHelp(out, fs) }
	return fs, &opts
}

func runSnapshotImage(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSnapshotImageHelp(os.Stdout)
		return nil
	}

	switch args[0] {
	case "export":
		if len(args) >= 2 && (args[1] == "-h" || args[1] == "--help") {
			fs, _ := newSnapshotImageExportFlagSet(os.Stdout)
			printSnapshotImageExportHelp(os.Stdout, fs)
			return nil
		}
		return runSnapshotImageExport(ctx, args[1:])
	default:
		printSnapshotImageHelp(os.Stderr)
		return fmt.Errorf("unknown snapshot-image command %q", args[0])
	}
}

func runSnapshots(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSnapshotsHelp(os.Stdout)
		return nil
	}

	switch args[0] {
	case "ls":
		return runSnapshotList(ctx, args[1:])
	case "rm":
		return runSnapshotRemove(ctx, args[1:])
	default:
		printSnapshotsHelp(os.Stderr)
		return fmt.Errorf("unknown snapshots command %q", args[0])
	}
}

func runSnapshotImageExport(ctx context.Context, args []string) error {
	opts, err := parseSnapshotImageExportArgs(args)
	if err != nil {
		return err
	}

	res, err := image.ExportSnapshot(ctx, image.SnapshotExportRequest{
		BootIndexTag:     opts.tag,
		ConfigPath:       opts.configPath,
		Namespace:        opts.namespace,
		RootfsSnapshotID: opts.snapshotID,
		SandboxID:        opts.sandboxID,
	})
	if err != nil {
		return fmt.Errorf("conch snapshot-image export: %w", err)
	}

	fmt.Println("------------------------------------------------------------")
	fmt.Printf("Sandbox snapshot image: %s\n", res.BootIndexTag)
	fmt.Printf("Image digest: %s\n", res.BootIndexDigest)
	return nil
}

func parseSnapshotImageExportArgs(args []string) (snapshotExportOptions, error) {
	fs, opts := newSnapshotImageExportFlagSet(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return snapshotExportOptions{}, err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return snapshotExportOptions{}, fmt.Errorf("conch snapshot-image export: unexpected positional arguments: %v", fs.Args())
	}
	if opts.tag == "" {
		fs.Usage()
		return snapshotExportOptions{}, fmt.Errorf("conch snapshot-image export: output tag is required")
	}
	if (opts.snapshotID == "") == (opts.sandboxID == "") {
		fs.Usage()
		return snapshotExportOptions{}, fmt.Errorf("conch snapshot-image export: exactly one of --snapshot-id or --sandbox-id is required")
	}
	return *opts, nil
}

func runSnapshotList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("snapshots ls", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	namespace := fs.String("namespace", "", "containerd namespace")
	configPath := fs.String("config", "", "config file path")
	var filters stringSliceFlag
	fs.StringVar(namespace, "n", "", "containerd namespace")
	fs.Var(&filters, "filter", "containerd snapshot filter")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("conch snapshots ls: unexpected positional arguments: %v", fs.Args())
	}
	cfg, err := loadConchConfig(*configPath)
	if err != nil {
		return fmt.Errorf("conch snapshots ls: load config: %w", err)
	}
	snapshots, err := client.NewClientWithConfig("", *configPath).ListSnapshots(ctx, client.ListSnapshotsRequest{
		Namespace: resolveConchNamespace(cfg, *namespace),
		Filters:   filters,
	})
	if err != nil {
		return fmt.Errorf("conch snapshots ls: %w", err)
	}
	fmt.Printf("%-12s %-64s %s\n", "KIND", "KEY", "PARENT")
	for _, snapshot := range snapshots {
		fmt.Printf("%-12s %-64s %s\n", snapshot.Kind, snapshot.Key, snapshot.Parent)
	}
	return nil
}

func runSnapshotRemove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("snapshots rm", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	namespace := fs.String("namespace", "", "containerd namespace")
	configPath := fs.String("config", "", "config file path")
	cascade := fs.Bool("cascade", false, "remove the whole Conch rootfs/mem/vm snapshot group")
	fs.StringVar(namespace, "n", "", "containerd namespace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("conch snapshots rm: exactly one snapshot key is required")
	}
	cfg, err := loadConchConfig(*configPath)
	if err != nil {
		return fmt.Errorf("conch snapshots rm: load config: %w", err)
	}
	key := fs.Arg(0)
	if err := client.NewClientWithConfig("", *configPath).RemoveSnapshot(ctx, client.RemoveSnapshotRequest{
		Key:       key,
		Namespace: resolveConchNamespace(cfg, *namespace),
		Cascade:   *cascade,
	}); err != nil {
		return fmt.Errorf("conch snapshots rm: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Removed snapshot: %s\n", key)
	return nil
}
