package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/openeuler/Conch/internal/cli/client"
)

func printSandboxHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch sandbox <command> [options]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Commands:")
	fmt.Fprintln(out, "  create      Create a sandbox from a Template ID or the daemon default.")
	fmt.Fprintln(out, "  checkpoint  Checkpoint a sandbox into a resumable template.")
	fmt.Fprintln(out, "  suspend     Suspend a running sandbox.")
	fmt.Fprintln(out, "  resume      Resume a suspended sandbox.")
	fmt.Fprintln(out, "  delete      Delete a sandbox.")
	fmt.Fprintln(out, "  ls          List sandboxes.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Run 'conch sandbox <command> --help' for command-specific usage.")
}

func PrintSandboxCreateHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch sandbox create [--template-id <template-id>] [options]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Description:")
	fmt.Fprintln(out, "  Create a sandbox from a Template ID. A Template ID is its Boot Index digest.")
	fmt.Fprintln(out, "  Unset template and resource fields use conchd sandbox.default_spec.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  --template-id string")
	fmt.Fprintln(out, "        Template ID (default: conchd sandbox.default_spec.template_id)")
	fmt.Fprintln(out, "  --sandbox-id string")
	fmt.Fprintln(out, "        sandbox ID (default: generated)")
	fmt.Fprintln(out, "  --ram-mb int")
	fmt.Fprintln(out, "        memory size in MiB (default: conchd sandbox.default_spec.ram_mb)")
	fmt.Fprintln(out, "  --config string")
	fmt.Fprintln(out, "        config file path")
}

func RunSandbox(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printSandboxHelp(os.Stdout)
		return nil
	}
	switch args[0] {
	case "create":
		if len(args) >= 2 && (args[1] == "-h" || args[1] == "--help") {
			PrintSandboxCreateHelp(os.Stdout)
			return nil
		}
		return runSandboxCreate(ctx, args[1:])
	case "checkpoint":
		return runSandboxCheckpoint(ctx, args[1:])
	case "suspend":
		return runSandboxLifecycle(ctx, args[1:], "suspend")
	case "resume":
		return runSandboxLifecycle(ctx, args[1:], "resume")
	case "delete":
		return runSandboxDelete(ctx, args[1:])
	case "ls", "list":
		return runSandboxList(ctx, args[1:])
	default:
		printSandboxHelp(os.Stderr)
		return fmt.Errorf("unknown sandbox command %q", args[0])
	}
}

func runSandboxDelete(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sandbox delete", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("conch sandbox delete: exactly one sandbox ID is required")
	}
	c, err := client.New(client.Options{ConfigPath: *configPath})
	if err != nil {
		return fmt.Errorf("conch sandbox delete: create API client: %w", err)
	}
	id := fs.Arg(0)
	if err := c.DeleteSandbox(ctx, id); err != nil {
		return fmt.Errorf("conch sandbox delete: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Deleted sandbox: %s\n", id)
	return nil
}

func runSandboxList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sandbox ls", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("conch sandbox ls: unexpected positional arguments: %v", fs.Args())
	}
	c, err := client.New(client.Options{ConfigPath: *configPath})
	if err != nil {
		return fmt.Errorf("conch sandbox ls: create API client: %w", err)
	}
	records, err := c.ListSandboxes(ctx)
	if err != nil {
		return fmt.Errorf("conch sandbox ls: %w", err)
	}
	return printSandboxList(os.Stdout, records)
}

func printSandboxList(out io.Writer, records []client.SandboxRecord) error {
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].SandboxID < records[j].SandboxID
	})
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTEMPLATE_ID\tCPU\tMEMORY_MB\tSTARTED_AT")
	for _, record := range records {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n",
			record.SandboxID, record.TemplateID,
			record.CPUCount, record.MemoryMB, record.StartedAt)
	}
	return tw.Flush()
}

func runSandboxCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sandbox create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	templateID := fs.String("template-id", "", "Template ID (uses daemon default if omitted)")
	sandboxID := fs.String("sandbox-id", "", "sandbox ID")
	configPath := fs.String("config", "", "config file path")
	ramMB := fs.Int64("ram-mb", 0, "memory size in MiB")
	fs.Usage = func() { PrintSandboxCreateHelp(os.Stderr) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("conch sandbox create: unexpected positional arguments: %v", fs.Args())
	}
	if *ramMB < 0 {
		return fmt.Errorf("conch sandbox create: --ram-mb must not be negative")
	}
	id := *sandboxID
	if id == "" {
		id = fmt.Sprintf("sandbox-%d", time.Now().UnixNano())
	}
	conchClient, err := client.New(client.Options{ConfigPath: *configPath})
	if err != nil {
		return fmt.Errorf("conch sandbox create: create API client: %w", err)
	}
	if _, err := conchClient.CreateSandbox(ctx, client.SandboxCreateRequest{
		TemplateID: *templateID,
		SandboxID:  id,
		RAMMB:      *ramMB,
	}); err != nil {
		return fmt.Errorf("conch sandbox create: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Sandbox: %s\n", id)
	return nil
}

func runSandboxCheckpoint(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sandbox checkpoint", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("conch sandbox checkpoint: exactly one sandbox ID is required")
	}
	conchClient, err := client.New(client.Options{ConfigPath: *configPath})
	if err != nil {
		return fmt.Errorf("conch sandbox checkpoint: create API client: %w", err)
	}
	checkpoint, err := conchClient.CheckpointSandbox(ctx, client.SandboxCheckpointRequest{
		SandboxID: fs.Arg(0),
	})
	if err != nil {
		return fmt.Errorf("conch sandbox checkpoint: %w", err)
	}
	if checkpoint.Status != "ok" {
		return fmt.Errorf("conch sandbox checkpoint: unexpected status %q", checkpoint.Status)
	}
	fmt.Fprintf(os.Stdout, "Template ID: %s\n", checkpoint.TemplateID)
	return nil
}

func runSandboxLifecycle(ctx context.Context, args []string, op string) error {
	fs := flag.NewFlagSet("sandbox "+op, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("conch sandbox %s: exactly one sandbox ID is required", op)
	}
	c, err := client.New(client.Options{ConfigPath: *configPath})
	if err != nil {
		return fmt.Errorf("conch sandbox %s: create API client: %w", op, err)
	}
	id := fs.Arg(0)
	req := client.SandboxLifecycleRequest{SandboxID: id}
	switch op {
	case "suspend":
		err = c.SuspendSandbox(ctx, req)
	case "resume":
		err = c.ResumeSandbox(ctx, req)
	}
	if err != nil {
		return fmt.Errorf("conch sandbox %s: %w", op, err)
	}
	fmt.Fprintf(os.Stdout, "%s sandbox: %s\n", op, id)
	return nil
}
