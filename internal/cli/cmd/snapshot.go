package cmd

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/openeuler/Conch/internal/image/client"
)

func PrintSnapshotHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch debug snapshot ls [options]")
	fmt.Fprintln(out, "  conch debug snapshot rm [options] <snapshot-key>")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Subcommands:")
	fmt.Fprintln(out, "  ls      List EROFS snapshots from conchd/containerd.")
	fmt.Fprintln(out, "  rm      Remove one EROFS snapshot from conchd/containerd.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Common usage:")
	fmt.Fprintln(out, "  conch debug snapshot ls")
	fmt.Fprintln(out, "  conch debug snapshot rm <snapshot-key>")
}

func runSnapshot(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		PrintSnapshotHelp(os.Stdout)
		return nil
	}

	switch args[0] {
	case "ls":
		return runSnapshotList(ctx, args[1:])
	case "rm":
		return runSnapshotRemove(ctx, args[1:])
	default:
		PrintSnapshotHelp(os.Stderr)
		return fmt.Errorf("unknown debug snapshot command %q", args[0])
	}
}

func runSnapshotList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("debug snapshot ls", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "config file path")
	var filters stringSliceFlag
	fs.Var(&filters, "filter", "containerd snapshot filter")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("conch debug snapshot ls: unexpected positional arguments: %v", fs.Args())
	}
	conchClient, err := client.NewClientWithConfig("", *configPath)
	if err != nil {
		return fmt.Errorf("conch debug snapshot ls: %w", err)
	}
	snapshots, err := conchClient.ListSnapshots(ctx, client.ListSnapshotsRequest{
		Filters: filters,
	})
	if err != nil {
		return fmt.Errorf("conch debug snapshot ls: %w", err)
	}
	fmt.Fprintf(os.Stdout, "%-12s %-64s %-64s\n", "KIND", "KEY", "PARENT")
	for _, snapshot := range snapshots {
		fmt.Fprintf(os.Stdout, "%-12s %-64s %-64s\n",
			snapshot.Kind,
			snapshot.Key,
			cmp.Or(snapshot.Parent, "-"),
		)
	}
	return nil
}

func runSnapshotRemove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("debug snapshot rm", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("conch debug snapshot rm: exactly one snapshot key is required")
	}
	key := fs.Arg(0)
	conchClient, err := client.NewClientWithConfig("", *configPath)
	if err != nil {
		return fmt.Errorf("conch debug snapshot rm: %w", err)
	}
	if err := conchClient.RemoveSnapshot(ctx, client.RemoveSnapshotRequest{
		Key: key,
	}); err != nil {
		return fmt.Errorf("conch debug snapshot rm: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Removed snapshot: %s\n", key)
	return nil
}
