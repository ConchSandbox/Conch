package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/openeuler/Conch/internal/image/client"
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
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Run 'conch sandbox <command> --help' for command-specific usage.")
}

func PrintSandboxCreateHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch sandbox create [--template-id <template-id>] [options]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Description:")
	fmt.Fprintln(out, "  Create a sandbox from a Template ID. If omitted, conchd uses")
	fmt.Fprintln(out, "  sandbox.default_template_id. Other unset resources also use conchd defaults.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  --template-id string")
	fmt.Fprintln(out, "        template ID (default: conchd sandbox.default_template_id)")
	fmt.Fprintln(out, "  --sandbox-id string")
	fmt.Fprintln(out, "        sandbox ID (default: generated)")
	fmt.Fprintln(out, "  --ram-mb int")
	fmt.Fprintln(out, "        memory size in MiB (default: conchd sandbox.default_ram_mb)")
	fmt.Fprintln(out, "  -n, --namespace string")
	fmt.Fprintln(out, "        containerd namespace")
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
	default:
		printSandboxHelp(os.Stderr)
		return fmt.Errorf("unknown sandbox command %q", args[0])
	}
}

func runSandboxCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sandbox create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	templateID := fs.String("template-id", "", "template ID (uses daemon default if omitted)")
	sandboxID := fs.String("sandbox-id", "", "sandbox ID")
	namespace := fs.String("namespace", "", "containerd namespace")
	configPath := fs.String("config", "", "config file path")
	ramMB := fs.Int64("ram-mb", 0, "memory size in MiB")
	fs.StringVar(namespace, "n", "", "containerd namespace")
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
	cfg, err := LoadConchConfig(*configPath)
	if err != nil {
		return fmt.Errorf("conch sandbox create: load config: %w", err)
	}
	id := *sandboxID
	if id == "" {
		id = fmt.Sprintf("sandbox-%d", time.Now().UnixNano())
	}
	if err := client.NewClientWithConfig("", *configPath).CreateSandbox(*templateID, id, ResolveConchNamespace(cfg, *namespace), *ramMB); err != nil {
		return fmt.Errorf("conch sandbox create: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Sandbox: %s\n", id)
	return nil
}

func runSandboxCheckpoint(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sandbox checkpoint", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	namespace := fs.String("namespace", "", "containerd namespace")
	configPath := fs.String("config", "", "config file path")
	fs.StringVar(namespace, "n", "", "containerd namespace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("conch sandbox checkpoint: exactly one sandbox ID is required")
	}
	cfg, err := LoadConchConfig(*configPath)
	if err != nil {
		return fmt.Errorf("conch sandbox checkpoint: load config: %w", err)
	}
	templateID, err := client.NewClientWithConfig("", *configPath).CheckpointSandbox(ctx, fs.Arg(0), ResolveConchNamespace(cfg, *namespace))
	if err != nil {
		return fmt.Errorf("conch sandbox checkpoint: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Template: %s\n", templateID)
	return nil
}

func runSandboxLifecycle(ctx context.Context, args []string, op string) error {
	fs := flag.NewFlagSet("sandbox "+op, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	namespace := fs.String("namespace", "", "containerd namespace")
	configPath := fs.String("config", "", "config file path")
	fs.StringVar(namespace, "n", "", "containerd namespace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("conch sandbox %s: exactly one sandbox ID is required", op)
	}
	cfg, err := LoadConchConfig(*configPath)
	if err != nil {
		return fmt.Errorf("conch sandbox %s: load config: %w", op, err)
	}
	c := client.NewClientWithConfig("", *configPath)
	ns := ResolveConchNamespace(cfg, *namespace)
	id := fs.Arg(0)
	switch op {
	case "suspend":
		err = c.SuspendSandbox(ctx, id, ns)
	case "resume":
		err = c.ResumeSandbox(ctx, id, ns)
	}
	if err != nil {
		return fmt.Errorf("conch sandbox %s: %w", op, err)
	}
	fmt.Fprintf(os.Stdout, "%s sandbox: %s\n", op, id)
	return nil
}
