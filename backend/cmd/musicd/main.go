// musicd is the only backend process entry point.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/gitsang/music-transcription/backend/internal/core"
	"github.com/gitsang/music-transcription/backend/internal/httpapi"
	"github.com/gitsang/music-transcription/backend/internal/tasks"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("musicd stopped", slog.Any("err", err))
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: musicd api|worker|dispatcher|migrate|init-admin|health")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger.With(slog.String("service", args[0])))
	cfg, err := core.LoadConfig()
	if err != nil {
		return err
	}
	store, err := core.Open(cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	switch args[0] {
	case "migrate":
		return store.Migrate(ctx)
	case "init-admin":
		flags := flag.NewFlagSet("init-admin", flag.ContinueOnError)
		username := flags.String("username", "", "administrator username")
		passwordStdin := flags.Bool("password-stdin", false, "read password from stdin")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if strings.TrimSpace(*username) == "" {
			return errors.New("--username is required")
		}
		password, err := readPassword(*passwordStdin)
		if err != nil {
			return err
		}
		id, err := core.InitAdmin(store, *username, password)
		if err != nil {
			return err
		}
		fmt.Println("Administrator created:", id)
		return nil
	case "api":
		return serve(ctx, cfg, httpapi.New(store))
	case "worker", "dispatcher":
		runner, err := tasks.New(store)
		if err != nil {
			return err
		}
		defer runner.Close()
		if args[0] == "worker" {
			return runner.Work(ctx)
		}
		return dispatch(ctx, runner)
	case "health":
		flags := flag.NewFlagSet("health", flag.ContinueOnError)
		role := flags.String("role", "api", "api, worker or dispatcher")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		result := store.Health(ctx, *role)
		encoded, _ := json.Marshal(result)
		fmt.Println(string(encoded))
		if result["status"] != "ready" {
			os.Exit(1)
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func serve(ctx context.Context, cfg core.Config, handler http.Handler) error {
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No global write timeout: authorized downloads are long-lived.
	}
	errorsSeen := make(chan error, 1)
	go func() { errorsSeen <- server.ListenAndServe() }()
	slog.Info("api listening", slog.String("addr", cfg.ListenAddr))
	select {
	case err := <-errorsSeen:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

func dispatch(ctx context.Context, runner *tasks.Runner) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := runner.Dispatch(ctx); err != nil && ctx.Err() == nil {
			slog.Error("dispatcher tick failed", slog.Any("err", err))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func readPassword(fromStdin bool) (string, error) {
	if fromStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, os.ErrClosed) && line == "" {
			return "", errors.New("password was not provided on stdin")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	file, err := os.Open("/dev/tty")
	if err != nil {
		return "", errors.New("no terminal for password prompt; use --password-stdin")
	}
	defer file.Close()
	fmt.Fprint(file, "Administrator password (minimum 12 characters): ")
	first, err := term.ReadPassword(int(file.Fd()))
	fmt.Fprintln(file)
	if err != nil {
		return "", err
	}
	fmt.Fprint(file, "Confirm password: ")
	second, err := term.ReadPassword(int(file.Fd()))
	fmt.Fprintln(file)
	if err != nil {
		return "", err
	}
	if string(first) != string(second) {
		return "", errors.New("password confirmation does not match")
	}
	return string(first), nil
}
