package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/openeuler/Conch/internal/cow"
)

type commandOptions struct {
	socketPath string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Printf("conch-cow: %v", err)
		os.Exit(1)
	}
}

func parseOptions(args []string) (commandOptions, bool, error) {
	flags := flag.NewFlagSet("conch-cow", flag.ContinueOnError)
	flags.SetOutput(os.Stdout)
	flags.Usage = func() {
		_, _ = fmt.Fprintln(flags.Output(), "Usage: conch-cow [--socket PATH]")
		_, _ = fmt.Fprintln(flags.Output(), "Serve on-demand Guest memory pages for Conch.")
		flags.PrintDefaults()
	}
	socketPath := flags.String("socket", cow.DefaultSocketPath, "Unix control socket path")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return commandOptions{}, true, nil
		}
		return commandOptions{}, false, err
	}
	if flags.NArg() != 0 {
		return commandOptions{}, false, fmt.Errorf("positional arguments are not accepted: %v", flags.Args())
	}
	return commandOptions{socketPath: *socketPath}, false, nil
}

func run(ctx context.Context, args []string) error {
	options, help, err := parseOptions(args)
	if err != nil || help {
		return err
	}
	server := cow.NewServer(options.socketPath)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx) }()

	select {
	case <-server.Ready():
	case serveErr := <-serveDone:
		return errors.Join(serveErr, server.Close())
	case <-ctx.Done():
		closeErr := server.Close()
		return errors.Join(closeErr, <-serveDone)
	}

	select {
	case serveErr := <-serveDone:
		return errors.Join(serveErr, server.Close())
	case <-ctx.Done():
		closeErr := server.Close()
		return errors.Join(closeErr, <-serveDone)
	}
}
