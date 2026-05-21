package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/openeuler/Conch/internal/image/conchconvert"
)

type convertOptions struct {
	source     string
	kernel     string
	initrd     string
	tag        string
	namespace  string
	configPath string
	apiURL     string
	address    string
	plainHTTP  bool
	user       string
	snapshot   bool
}

func printConvertHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch convert [options]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Description:")
	fmt.Fprintln(out, "  Convert an existing OCI rootfs image plus kernel/initrd files into")
	fmt.Fprintln(out, "  a Conch native EROFS boot image.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  --source string")
	fmt.Fprintln(out, "        OCI rootfs image reference; reuse local conchd/containerd image first, pull if missing")
	fmt.Fprintln(out, "  --kernel string")
	fmt.Fprintln(out, "        kernel file path")
	fmt.Fprintln(out, "  --initrd string")
	fmt.Fprintln(out, "        initrd file path")
	fmt.Fprintln(out, "  -t, --tag string")
	fmt.Fprintln(out, "        output Conch image tag")
	fmt.Fprintln(out, "  --snapshot")
	fmt.Fprintln(out, "        boot once, pause, and include mem-snapshot component")
	fmt.Fprintln(out, "  -n, --namespace string")
	fmt.Fprintln(out, "        containerd namespace (default: config containerd.default_namespace or default)")
	fmt.Fprintln(out, "  -api-url string")
	fmt.Fprintln(out, "        conchd API base URL (default: config server endpoint or http://localhost:4063)")
	fmt.Fprintln(out, "  -address string")
	fmt.Fprintln(out, "        deprecated alias for -api-url")
	fmt.Fprintln(out, "  -config string")
	fmt.Fprintln(out, "        config file path (default: auto-detect common config paths)")
	fmt.Fprintln(out, "  --plain-http")
	fmt.Fprintln(out, "        allow plain HTTP / disable TLS verification for registry source pulls")
	fmt.Fprintln(out, "  --user string")
	fmt.Fprintln(out, "        registry credentials in username:password format for registry source pulls")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Examples:")
	fmt.Fprintln(out, "  conch convert --source docker.io/library/nginx:latest --kernel ./bzImage --initrd ./conch.initrd -t localhost/conch/nginx:latest")
}

func runConvert(ctx context.Context, args []string) error {
	opts, err := parseConvertArgs(args)
	if err != nil {
		return err
	}
	username, password, err := parseRegistryUser(opts.user)
	if err != nil {
		return fmt.Errorf("conch convert: %w", err)
	}

	res, err := conchconvert.Convert(ctx, conchconvert.ConvertOpts{
		Source:          opts.source,
		KernelPath:      opts.kernel,
		InitrdPath:      opts.initrd,
		BootIndexTag:    opts.tag,
		Namespace:       opts.namespace,
		ConfigPath:      opts.configPath,
		ConchAPIBaseURL: resolveConchAPIURL(opts.apiURL, opts.address),
		PlainHTTP:       opts.plainHTTP,
		Username:        username,
		Password:        password,
		Snapshot:        opts.snapshot,
		Out:             os.Stdout,
	})
	if err != nil {
		return fmt.Errorf("conch convert: %w", err)
	}
	printConvertSummary(os.Stdout, res)
	return nil
}

func parseConvertArgs(args []string) (convertOptions, error) {
	fs := flag.NewFlagSet("convert", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var opts convertOptions
	fs.StringVar(&opts.source, "source", "", "registry image reference for source OCI rootfs image")
	fs.StringVar(&opts.kernel, "kernel", "", "kernel file path")
	fs.StringVar(&opts.initrd, "initrd", "", "initrd file path")
	fs.StringVar(&opts.tag, "t", "", "output Conch image tag")
	fs.StringVar(&opts.tag, "tag", "", "output Conch image tag")
	fs.BoolVar(&opts.snapshot, "snapshot", false, "include mem-snapshot component by creating and pausing a sandbox")
	fs.StringVar(&opts.namespace, "namespace", "", "containerd namespace")
	fs.StringVar(&opts.namespace, "n", "", "containerd namespace")
	fs.StringVar(&opts.apiURL, "api-url", "", "conchd API base URL")
	fs.StringVar(&opts.address, "address", "", "deprecated alias for -api-url")
	fs.StringVar(&opts.configPath, "config", "", "config file path")
	fs.BoolVar(&opts.plainHTTP, "plain-http", false, "allow plain HTTP / disable TLS verification for registry source pulls")
	fs.StringVar(&opts.user, "user", "", "registry credentials in username:password format for registry source pulls")
	fs.Usage = func() { printConvertHelp(os.Stderr) }
	if err := fs.Parse(args); err != nil {
		return convertOptions{}, err
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return convertOptions{}, fmt.Errorf("conch convert: unexpected positional arguments: %v", fs.Args())
	}
	if opts.source == "" {
		fs.Usage()
		return convertOptions{}, fmt.Errorf("conch convert: --source is required")
	}
	if opts.kernel == "" {
		fs.Usage()
		return convertOptions{}, fmt.Errorf("conch convert: --kernel is required")
	}
	if opts.initrd == "" {
		fs.Usage()
		return convertOptions{}, fmt.Errorf("conch convert: --initrd is required")
	}
	if opts.tag == "" {
		fs.Usage()
		return convertOptions{}, fmt.Errorf("conch convert: output tag is required")
	}
	return opts, nil
}

func printConvertSummary(out io.Writer, res conchconvert.Result) {
	fmt.Fprintln(out, "Convert outputs:")
	if res.SourceImageRef != "" {
		fmt.Fprintf(out, "  %-20s %s\n", "Source rootfs image:", res.SourceImageRef)
	}
	if res.RootfsImageRef != "" {
		fmt.Fprintf(out, "  %-20s %s\n", "Native rootfs image:", res.RootfsImageRef)
	}
	if res.KernelImageRef != "" {
		fmt.Fprintf(out, "  %-20s %s\n", "Kernel component:", res.KernelImageRef)
	}
	if res.BootIndexTag != "" {
		fmt.Fprintf(out, "  %-20s %s\n", "Conch image:", res.BootIndexTag)
		fmt.Fprintf(out, "  %-20s %s\n", "Push command:", fmt.Sprintf("conch push %s <registry>/<repository>:<tag>", res.BootIndexTag))
	}
	if res.BootIndexDigest != "" {
		fmt.Fprintf(out, "  %-20s %s\n", "Image digest:", res.BootIndexDigest)
	}
}
