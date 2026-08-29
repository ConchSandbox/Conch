package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/openeuler/Conch/internal/cli/client"
)

type templateCreateOptions struct {
	name       string
	source     string
	kernel     string
	initrd     string
	configPath string
	apiURL     string
	address    string
	plainHTTP  bool
	username   string
	password   string
	user       string
}

type templateRegistryOptions struct {
	configPath string
	apiURL     string
	address    string
	plainHTTP  bool
	username   string
	password   string
	user       string
	timeout    string
}

func printTemplateHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch template <command> [options]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Commands:")
	fmt.Fprintln(out, "  create   Build a template from an OCI image, kernel, and initrd.")
	fmt.Fprintln(out, "  pull     Pull a registry Boot Index into a local Template.")
	fmt.Fprintln(out, "  push     Push a Template Boot Index to a registry.")
	fmt.Fprintln(out, "  unpack   Unpack a Template's Boot Index into snapshots.")
	fmt.Fprintln(out, "  ls       List templates.")
	fmt.Fprintln(out, "  inspect  Inspect a template.")
	fmt.Fprintln(out, "  rm       Remove a template.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Run 'conch template <command> --help' for command-specific usage.")
}

func PrintTemplateCreateHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch template create --name <name> --source <image> --kernel <path> --initrd <path> [options]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Description:")
	fmt.Fprintln(out, "  Convert an existing OCI rootfs image plus kernel/initrd files into")
	fmt.Fprintln(out, "  a bootable Conch Template.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  --name string")
	fmt.Fprintln(out, "        mutable logical Template Name (mapped to an internal containerd image record)")
	fmt.Fprintln(out, "  --source string")
	fmt.Fprintln(out, "        OCI rootfs image reference; reuse local conchd/containerd image first, pull if missing")
	fmt.Fprintln(out, "  --kernel string")
	fmt.Fprintln(out, "        kernel file path")
	fmt.Fprintln(out, "  --initrd string")
	fmt.Fprintln(out, "        initrd file path")
	fmt.Fprintln(out, "  -api-url string")
	fmt.Fprintln(out, "        conchd API base URL (default: conchd unix socket from config)")
	fmt.Fprintln(out, "  -address string")
	fmt.Fprintln(out, "        deprecated alias for -api-url")
	fmt.Fprintln(out, "  -config string")
	fmt.Fprintln(out, "        config file path (default: auto-detect common config paths)")
	fmt.Fprintln(out, "  --plain-http")
	fmt.Fprintln(out, "        allow plain HTTP / disable TLS verification for registry source pulls")
	fmt.Fprintln(out, "  --user string")
	fmt.Fprintln(out, "        registry credentials in username:password format for registry source pulls")
	fmt.Fprintln(out, "  --username string")
	fmt.Fprintln(out, "        registry username")
	fmt.Fprintln(out, "  --password string")
	fmt.Fprintln(out, "        registry password")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Examples:")
	fmt.Fprintln(out, "  conch template create --name localhost/conch/nginx:latest --source docker.io/library/nginx:latest --kernel ./bzImage --initrd ./conch.initrd")
}

func RunTemplate(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printTemplateHelp(os.Stdout)
		return nil
	}
	switch args[0] {
	case "create":
		if len(args) >= 2 && (args[1] == "-h" || args[1] == "--help") {
			PrintTemplateCreateHelp(os.Stdout)
			return nil
		}
		return RunTemplateCreate(ctx, args[1:])
	case "pull":
		return runTemplatePull(ctx, args[1:])
	case "push":
		return runTemplatePush(ctx, args[1:])
	case "unpack":
		if len(args) >= 2 && (args[1] == "-h" || args[1] == "--help") {
			PrintTemplateUnpackHelp(os.Stdout)
			return nil
		}
		return runTemplateUnpack(ctx, args[1:])
	case "ls":
		return runTemplateList(ctx, args[1:])
	case "inspect":
		return runTemplateInspect(ctx, args[1:])
	case "rm":
		return runTemplateRemove(ctx, args[1:])
	default:
		printTemplateHelp(os.Stderr)
		return fmt.Errorf("unknown template command %q", args[0])
	}
}

func runTemplatePull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("template pull", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var opts templateRegistryOptions
	registerTemplateRegistryFlags(fs, &opts, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("conch template pull: exactly one registry reference is required")
	}
	username, password, err := templateRegistryCredentials(opts)
	if err != nil {
		return fmt.Errorf("conch template pull: %w", err)
	}
	conchClient, err := client.New(client.Options{
		BaseURL:    ResolveConchAPIURL(opts.apiURL, opts.address),
		ConfigPath: opts.configPath,
	})
	if err != nil {
		return fmt.Errorf("conch template pull: create API client: %w", err)
	}
	result, err := conchClient.PullTemplate(ctx, client.TemplatePullRequest{
		Reference: fs.Arg(0),
		PlainHTTP: opts.plainHTTP,
		Username:  username,
		Password:  password,
	})
	if err != nil {
		return fmt.Errorf("conch template pull: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Template Name: %s\n", result.TemplateName)
	fmt.Fprintf(os.Stdout, "Template ID: %s\n", result.TemplateID)
	return nil
}

func runTemplatePush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("template push", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var opts templateRegistryOptions
	registerTemplateRegistryFlags(fs, &opts, true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("conch template push: exactly two arguments are required: <template-name> <remote-reference>")
	}
	username, password, err := templateRegistryCredentials(opts)
	if err != nil {
		return fmt.Errorf("conch template push: %w", err)
	}
	var apiTimeout time.Duration
	if opts.timeout != "" {
		apiTimeout, err = time.ParseDuration(opts.timeout)
		if err != nil || apiTimeout <= 0 {
			return fmt.Errorf("conch template push: invalid --timeout %q", opts.timeout)
		}
	}
	conchClient, err := client.New(client.Options{
		BaseURL:    ResolveConchAPIURL(opts.apiURL, opts.address),
		ConfigPath: opts.configPath,
		Timeout:    apiTimeout,
	})
	if err != nil {
		return fmt.Errorf("conch template push: create API client: %w", err)
	}
	if err := conchClient.PushTemplate(ctx, client.TemplatePushRequest{
		Name:            fs.Arg(0),
		RemoteReference: fs.Arg(1),
		PlainHTTP:       opts.plainHTTP,
		Username:        username,
		Password:        password,
	}); err != nil {
		return fmt.Errorf("conch template push: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Pushed template: %s -> %s\n", fs.Arg(0), fs.Arg(1))
	return nil
}

func registerTemplateRegistryFlags(fs *flag.FlagSet, opts *templateRegistryOptions, push bool) {
	fs.StringVar(&opts.configPath, "config", "", "config file path")
	fs.StringVar(&opts.apiURL, "api-url", "", "conchd API base URL")
	fs.StringVar(&opts.address, "address", "", "deprecated alias for -api-url")
	fs.BoolVar(&opts.plainHTTP, "plain-http", false, "use plain HTTP for registry access")
	fs.StringVar(&opts.user, "user", "", "registry credentials in username:password format")
	fs.StringVar(&opts.username, "username", "", "registry username")
	fs.StringVar(&opts.password, "password", "", "registry password")
	if push {
		fs.StringVar(&opts.timeout, "timeout", "", "timeout for the registry push")
	}
}

func templateRegistryCredentials(opts templateRegistryOptions) (string, string, error) {
	if opts.user == "" {
		return opts.username, opts.password, nil
	}
	return ParseRegistryUser(opts.user)
}

func RunTemplateCreate(ctx context.Context, args []string) error {
	opts, err := parseTemplateCreateArgs(args)
	if err != nil {
		return err
	}
	if opts.user != "" {
		username, password, err := ParseRegistryUser(opts.user)
		if err != nil {
			return fmt.Errorf("conch template create: %w", err)
		}
		opts.username = username
		opts.password = password
	}
	return createTemplate(ctx, "conch template create", opts)
}

func parseTemplateCreateArgs(args []string) (templateCreateOptions, error) {
	fs := flag.NewFlagSet("template create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var opts templateCreateOptions
	registerTemplateCreateFlags(fs, &opts)
	fs.Usage = func() { PrintTemplateCreateHelp(os.Stderr) }
	if err := fs.Parse(args); err != nil {
		return templateCreateOptions{}, err
	}
	if fs.NArg() != 0 {
		return templateCreateOptions{}, fmt.Errorf("conch template create: unexpected positional arguments: %v", fs.Args())
	}
	if opts.name == "" || opts.source == "" || opts.kernel == "" || opts.initrd == "" {
		return templateCreateOptions{}, fmt.Errorf("conch template create: --name, --source, --kernel, and --initrd are required")
	}
	return opts, nil
}

func registerTemplateCreateFlags(fs *flag.FlagSet, opts *templateCreateOptions) {
	fs.StringVar(&opts.name, "name", "", "Template Name")
	fs.StringVar(&opts.source, "source", "", "source rootfs image")
	fs.StringVar(&opts.kernel, "kernel", "", "kernel file path")
	fs.StringVar(&opts.initrd, "initrd", "", "initrd file path")
	fs.StringVar(&opts.configPath, "config", "", "config file path")
	fs.StringVar(&opts.apiURL, "api-url", "", "conchd API base URL")
	fs.StringVar(&opts.address, "address", "", "deprecated alias for -api-url")
	fs.BoolVar(&opts.plainHTTP, "plain-http", false, "use plain HTTP for registry access")
	fs.StringVar(&opts.username, "username", "", "registry username")
	fs.StringVar(&opts.password, "password", "", "registry password")
	fs.StringVar(&opts.user, "user", "", "registry credentials in username:password format")
}

func createTemplate(ctx context.Context, command string, opts templateCreateOptions) error {
	conchClient, err := client.New(client.Options{
		BaseURL:    ResolveConchAPIURL(opts.apiURL, opts.address),
		ConfigPath: opts.configPath,
	})
	if err != nil {
		return fmt.Errorf("%s: create API client: %w", command, err)
	}
	res, err := conchClient.CreateTemplate(ctx, client.TemplateCreateRequest{
		Name:       opts.name,
		Source:     opts.source,
		KernelPath: opts.kernel,
		InitrdPath: opts.initrd,
		PlainHTTP:  opts.plainHTTP,
		Username:   opts.username,
		Password:   opts.password,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}
	printTemplateCreateSummary(os.Stdout, res)
	return nil
}

func printTemplateCreateSummary(out io.Writer, res client.TemplateCreateResponse) {
	if res.TemplateName != "" {
		fmt.Fprintf(out, "Template Name: %s\n", res.TemplateName)
	}
	if res.TemplateID != "" {
		fmt.Fprintf(out, "Template ID: %s\n", res.TemplateID)
	}
}

func runTemplateList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("template ls", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	origin := fs.String("origin", "", "template origin: image or checkpoint")
	bootMode := fs.String("boot-mode", "", "boot mode: cold or resume")
	configPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	conchClient, err := client.New(client.Options{ConfigPath: *configPath})
	if err != nil {
		return fmt.Errorf("conch template ls: create API client: %w", err)
	}
	items, err := conchClient.ListTemplates(ctx, client.TemplateListRequest{
		Origin:   *origin,
		BootMode: *bootMode,
	})
	if err != nil {
		return fmt.Errorf("conch template ls: %w", err)
	}
	printTemplates(os.Stdout, items)
	return nil
}

func runTemplateInspect(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("template inspect", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("conch template inspect: exactly one Template Name is required")
	}
	conchClient, err := client.New(client.Options{ConfigPath: *configPath})
	if err != nil {
		return fmt.Errorf("conch template inspect: create API client: %w", err)
	}
	item, err := conchClient.InspectTemplate(ctx, fs.Arg(0))
	if err != nil {
		return fmt.Errorf("conch template inspect: %w", err)
	}
	printTemplates(os.Stdout, []client.TemplateRecord{item})
	return nil
}

func runTemplateRemove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("template rm", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "config file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("conch template rm: exactly one Template Name is required")
	}
	name := fs.Arg(0)
	conchClient, err := client.New(client.Options{ConfigPath: *configPath})
	if err != nil {
		return fmt.Errorf("conch template rm: create API client: %w", err)
	}
	if err := conchClient.RemoveTemplate(ctx, name); err != nil {
		return fmt.Errorf("conch template rm: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Removed template: %s\n", name)
	return nil
}

func printTemplates(out io.Writer, items []client.TemplateRecord) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTEMPLATE_ID\tORIGIN\tBOOT_MODE\tSOURCE_REF\tSOURCE_SANDBOX")
	for _, item := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			displayTemplateValue(item.Name),
			displayTemplateValue(item.TemplateID),
			displayTemplateValue(item.Origin),
			displayTemplateValue(item.BootMode),
			displayTemplateValue(item.SourceRef),
			displayTemplateValue(item.SourceSandboxID),
		)
	}
	_ = tw.Flush()
}

func displayTemplateValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
