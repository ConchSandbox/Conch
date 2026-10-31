package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/openeuler/Conch/internal/cli/client"
)

func PrintTemplateSaveHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch template save [options] <template-name> [<template-name>...]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Description:")
	fmt.Fprintln(out, "  Save one or more Templates to a tar archive for offline transfer.")
	fmt.Fprintln(out, "  The archive contains the Boot Index and every blob needed to boot it.")
	fmt.Fprintln(out, "  -o or --output is required.")
	fmt.Fprintln(out, "  There is no default transfer time limit. Set --timeout or CONCH_API_TIMEOUT to cap it.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  -o, --output string")
	fmt.Fprintln(out, "        archive file to write")
	fmt.Fprintln(out, "  --timeout string")
	fmt.Fprintln(out, "        cap the archive transfer, for example 30m; unset means no limit")
	fmt.Fprintln(out, "  -api-url string")
	fmt.Fprintln(out, "        conchd API base URL (default: conchd unix socket from config)")
	fmt.Fprintln(out, "  -address string")
	fmt.Fprintln(out, "        deprecated alias for -api-url")
	fmt.Fprintln(out, "  -config string")
	fmt.Fprintln(out, "        config file path (default: auto-detect common config paths)")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Examples:")
	fmt.Fprintln(out, "  conch template save -o openeuler.tar localhost/conch/openeuler:latest")
}

func PrintTemplateLoadHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch template load [options]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Description:")
	fmt.Fprintln(out, "  Load Templates from a tar archive created by 'conch template save'.")
	fmt.Fprintln(out, "  Gzip and zstd compressed archive files are accepted.")
	fmt.Fprintln(out, "  -i or --input is required.")
	fmt.Fprintln(out, "  An existing Template Name is updated to the archived Boot Index.")
	fmt.Fprintln(out, "  There is no default transfer time limit. Set --timeout or CONCH_API_TIMEOUT to cap it.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  -i, --input string")
	fmt.Fprintln(out, "        archive file to load")
	fmt.Fprintln(out, "  --timeout string")
	fmt.Fprintln(out, "        cap the archive transfer, for example 30m; unset means no limit")
	fmt.Fprintln(out, "  -api-url string")
	fmt.Fprintln(out, "        conchd API base URL (default: conchd unix socket from config)")
	fmt.Fprintln(out, "  -address string")
	fmt.Fprintln(out, "        deprecated alias for -api-url")
	fmt.Fprintln(out, "  -config string")
	fmt.Fprintln(out, "        config file path (default: auto-detect common config paths)")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Examples:")
	fmt.Fprintln(out, "  conch template load -i openeuler.tar")
}

func runTemplateSave(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("template save", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var output string
	timeout := fs.String("timeout", "", "timeout for the archive transfer")
	apiURL := fs.String("api-url", "", "conchd API base URL")
	address := fs.String("address", "", "deprecated alias for -api-url")
	configPath := fs.String("config", "", "config file path")
	fs.StringVar(&output, "o", "", "archive file to write")
	fs.StringVar(&output, "output", "", "archive file to write")
	fs.Usage = func() { PrintTemplateSaveHelp(os.Stderr) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	output = strings.TrimSpace(output)
	if output == "" {
		fs.Usage()
		return fmt.Errorf("conch template save: -o or --output is required")
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return fmt.Errorf("conch template save: at least one Template Name is required")
	}
	apiTimeout, err := parseTimeoutFlag(*timeout)
	if err != nil {
		return fmt.Errorf("conch template save: %w", err)
	}
	conchClient, err := client.New(client.Options{
		BaseURL:          ResolveConchAPIURL(*apiURL, *address),
		ConfigPath:       *configPath,
		Timeout:          apiTimeout,
		NoDefaultTimeout: true,
	})
	if err != nil {
		return fmt.Errorf("conch template save: create API client: %w", err)
	}
	names := append([]string(nil), fs.Args()...)
	if err := saveTemplateArchive(output, func(w io.Writer) error {
		return conchClient.SaveTemplate(ctx, names, w)
	}); err != nil {
		return fmt.Errorf("conch template save: %w", err)
	}
	return nil
}

func runTemplateLoad(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("template load", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var input string
	timeout := fs.String("timeout", "", "timeout for the archive transfer")
	apiURL := fs.String("api-url", "", "conchd API base URL")
	address := fs.String("address", "", "deprecated alias for -api-url")
	configPath := fs.String("config", "", "config file path")
	fs.StringVar(&input, "i", "", "archive file to load")
	fs.StringVar(&input, "input", "", "archive file to load")
	fs.Usage = func() { PrintTemplateLoadHelp(os.Stderr) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return fmt.Errorf("conch template load: unexpected positional arguments: %v", fs.Args())
	}
	input = strings.TrimSpace(input)
	if input == "" {
		fs.Usage()
		return fmt.Errorf("conch template load: -i or --input is required")
	}
	apiTimeout, err := parseTimeoutFlag(*timeout)
	if err != nil {
		return fmt.Errorf("conch template load: %w", err)
	}
	file, err := os.Open(input)
	if err != nil {
		return fmt.Errorf("conch template load: open %s: %w", input, err)
	}
	defer file.Close()
	conchClient, err := client.New(client.Options{
		BaseURL:          ResolveConchAPIURL(*apiURL, *address),
		ConfigPath:       *configPath,
		Timeout:          apiTimeout,
		NoDefaultTimeout: true,
	})
	if err != nil {
		return fmt.Errorf("conch template load: create API client: %w", err)
	}
	results, err := conchClient.LoadTemplate(ctx, file)
	if err != nil {
		return fmt.Errorf("conch template load: %w", err)
	}
	for _, result := range results {
		fmt.Fprintf(os.Stdout, "Loaded image: %s\n", result.TemplateName)
	}
	return nil
}

func saveTemplateArchive(path string, write func(io.Writer) error) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary archive in %s: %w", dir, err)
	}
	tmp := file.Name()
	writeErr := write(file)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
