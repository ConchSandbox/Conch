package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/openeuler/Conch/internal/image/client"
	"github.com/openeuler/Conch/pkg/ulog"
)

func PrintImagePullHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  conch image pull [options] <image-name>")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Description:")
	fmt.Fprintln(out, "  Pull a Conch native image into containerd content store, then unpack")
	fmt.Fprintln(out, "  all child images and link snapshot labels.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Options:")
	fmt.Fprintln(out, "  -n, --namespace string")
	fmt.Fprintln(out, "        containerd namespace (default: config containerd.default_namespace or default)")
	fmt.Fprintln(out, "  -api-url string")
	fmt.Fprintln(out, "        conchd API base URL (default: config server endpoint or http://localhost:4063)")
	fmt.Fprintln(out, "  -address string")
	fmt.Fprintln(out, "        deprecated alias for -api-url")
	fmt.Fprintln(out, "  -config string")
	fmt.Fprintln(out, "        config file path (default: auto-detect common config paths)")
	fmt.Fprintln(out, "  --plain-http")
	fmt.Fprintln(out, "        allow plain HTTP / disable TLS verification for source image pulls")
	fmt.Fprintln(out, "  --user string")
	fmt.Fprintln(out, "        registry credentials in username:password format for source image pulls")
	fmt.Fprintln(out, "  --skip-unpack")
	fmt.Fprintln(out, "        pull image content without creating local snapshots")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Example:")
	fmt.Fprintln(out, "  conch image pull -n default hub.oepkgs.net/conch/sandbox-snapshot:latest")
	fmt.Fprintln(out, "  conch image pull --skip-unpack docker.io/library/nginx:latest")
	fmt.Fprintln(out, "  conch image pull --plain-http --user example-user:example-password docker.io/library/nginx:latest")
}

func RunImagePull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("image pull", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	apiURL := fs.String("api-url", "", "conchd API base URL")
	addr := fs.String("address", "", "deprecated alias for -api-url")
	namespace := fs.String("namespace", "", "containerd namespace")
	configPath := fs.String("config", "", "config file path")
	plainHTTP := fs.Bool("plain-http", false, "allow plain HTTP / disable TLS verification for source image pulls")
	user := fs.String("user", "", "registry credentials in username:password format for source image pulls")
	skipUnpack := fs.Bool("skip-unpack", false, "pull image content without creating local snapshots")
	fs.StringVar(namespace, "n", "", "containerd namespace")
	fs.Usage = func() { PrintImagePullHelp(os.Stderr) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("conch image pull: exactly one image name is required")
	}
	imageName := fs.Arg(0)

	cfg, err := LoadConchConfig(*configPath)
	if err != nil {
		return fmt.Errorf("conch image pull: load config: %w", err)
	}
	ns := ResolveConchNamespace(cfg, *namespace)
	username, password, err := ParseRegistryUser(*user)
	if err != nil {
		return fmt.Errorf("conch image pull: %w", err)
	}
	if !*skipUnpack {
		if err := InitUnpackLogger(); err != nil {
			return err
		}
		defer func() {
			logger := ulog.GetLogger()
			if closer, ok := logger.(interface{ Close() error }); ok {
				_ = closer.Close()
			}
		}()
	}

	conchClient := client.NewClientWithConfig(ResolveConchAPIURL(*apiURL, *addr), *configPath)
	fmt.Println("------------------------------------------------------------")
	fmt.Printf("Pulling image: %s\n", imageName)
	progressRenderer := newPullProgressRenderer(os.Stdout)
	results, err := conchClient.PullImageWithProgress(ctx, client.PullImageRequest{
		ImageName:  imageName,
		Namespace:  ns,
		PlainHTTP:  *plainHTTP,
		Username:   username,
		Password:   password,
		SkipUnpack: *skipUnpack,
	}, progressRenderer.Handle)
	if err != nil {
		return fmt.Errorf("conch image pull: %w", err)
	}
	if *skipUnpack {
		fmt.Printf("Pulled image without unpacking: %s\n", imageName)
		return nil
	}
	PrintUnpackSummary(results)
	return nil
}

const pullProgressBarWidth = 56
const (
	pullProgressFilled = "━"
	pullProgressEmpty  = "─"
	pullProgressGreen  = "\033[32m"
	pullProgressReset  = "\033[0m"
)

type pullProgressRenderer struct {
	out               io.Writer
	tty               bool
	components        map[string]client.PullProgressEvent
	printedLines      int
	unpackingRendered bool
}

func newPullProgressRenderer(out *os.File) *pullProgressRenderer {
	return &pullProgressRenderer{
		out:        out,
		tty:        isTerminal(out.Fd()),
		components: make(map[string]client.PullProgressEvent),
	}
}

func (r *pullProgressRenderer) Handle(event client.PullProgressEvent) {
	if r == nil {
		return
	}
	switch event.Status {
	case "started":
		return
	case "downloading":
		if r.unpackingRendered {
			return
		}
		if event.Component == "" || event.Total <= 0 {
			return
		}
		if event.Component == "overall" && r.hasPrimaryComponentProgress() {
			return
		}
		r.components[event.Component] = event
		r.renderComponents()
	case "unpacking":
		if !r.unpackingRendered {
			r.finishProgressArea()
			fmt.Fprintln(r.out, "Unpacking snapshots...")
			r.unpackingRendered = true
		}
	case "completed":
		r.finishProgressArea()
	case "error":
		r.finishProgressArea()
	}
}

func (r *pullProgressRenderer) renderComponents() {
	lines := r.componentLines()
	if r.tty && r.printedLines > 0 {
		fmt.Fprintf(r.out, "\033[%dA", r.printedLines)
	}
	for _, line := range lines {
		if r.tty {
			fmt.Fprint(r.out, "\r\033[K")
		}
		fmt.Fprintln(r.out, line)
	}
	r.printedLines = len(lines)
}

func (r *pullProgressRenderer) componentLines() []string {
	var lines []string
	components := []string{"rootfs", "kernel", "mem-snapshot"}
	if !r.hasPrimaryComponentProgress() {
		components = append(components, "overall")
	}
	for _, component := range components {
		event, ok := r.components[component]
		if !ok {
			continue
		}
		lines = append(lines, renderComponentProgress(component, event.Progress, event.Total, r.tty))
	}
	return lines
}

func (r *pullProgressRenderer) hasPrimaryComponentProgress() bool {
	for _, component := range []string{"rootfs", "kernel", "mem-snapshot"} {
		if _, ok := r.components[component]; ok {
			return true
		}
	}
	return false
}

func (r *pullProgressRenderer) finishProgressArea() {
	r.printedLines = 0
}

func renderComponentProgress(component string, progressBytes, totalBytes int64, color bool) string {
	if totalBytes <= 0 {
		return fmt.Sprintf("%-12s", component)
	}
	if progressBytes < 0 {
		progressBytes = 0
	}
	if progressBytes > totalBytes {
		progressBytes = totalBytes
	}
	filled := int(progressBytes * pullProgressBarWidth / totalBytes)
	filledBar := strings.Repeat(pullProgressFilled, filled)
	if color && filled > 0 {
		filledBar = pullProgressGreen + filledBar + pullProgressReset
	}
	bar := filledBar + strings.Repeat(pullProgressEmpty, pullProgressBarWidth-filled)
	percent := float64(progressBytes) * 100 / float64(totalBytes)
	return fmt.Sprintf("%-12s [%s] %5.1f%% %s / %s", component, bar, percent, formatPullBytes(progressBytes), formatPullBytes(totalBytes))
}

func formatPullBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for value := n / unit; value >= unit && exp < 4; value /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
