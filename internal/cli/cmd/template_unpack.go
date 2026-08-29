package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/openeuler/Conch/internal/cli/client"
)

func PrintTemplateUnpackHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch template unpack [options] <template-name>")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Description:")
	fmt.Fprintln(out, "  Unpack every component in a local Template's Boot Index.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  -api-url string")
	fmt.Fprintln(out, "        conchd API base URL (default: conchd unix socket from config)")
	fmt.Fprintln(out, "  -address string")
	fmt.Fprintln(out, "        deprecated alias for -api-url")
	fmt.Fprintln(out, "  -config string")
	fmt.Fprintln(out, "        config file path (default: auto-detect common config paths)")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Example:")
	fmt.Fprintln(out, "  conch template unpack registry.example.com/conch/template:latest")
}

func runTemplateUnpack(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("template unpack", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	apiURL := fs.String("api-url", "", "conchd API base URL")
	addr := fs.String("address", "", "deprecated alias for -api-url")
	configPath := fs.String("config", "", "config file path")
	fs.Usage = func() { PrintTemplateUnpackHelp(os.Stderr) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("conch template unpack: exactly one Template Name is required")
	}
	name := fs.Arg(0)

	conchClient, err := client.New(client.Options{
		BaseURL:    ResolveConchAPIURL(*apiURL, *addr),
		ConfigPath: *configPath,
	})
	if err != nil {
		return fmt.Errorf("conch template unpack: create API client: %w", err)
	}
	if err := conchClient.UnpackTemplate(ctx, client.TemplateUnpackRequest{
		Name: name,
	}); err != nil {
		return fmt.Errorf("conch template unpack: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Unpacked template: %s\n", name)
	return nil
}
