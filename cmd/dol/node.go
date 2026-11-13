package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/capacity"
	"github.com/fl0wo/docker-offload-lan/internal/config"
	"github.com/fl0wo/docker-offload-lan/internal/dockerx"
	"github.com/fl0wo/docker-offload-lan/internal/kindx"
	"github.com/fl0wo/docker-offload-lan/internal/node"
	"github.com/fl0wo/docker-offload-lan/internal/version"
	"github.com/spf13/cobra"
)

func newNodeCmd() *cobra.Command {
	c := &cobra.Command{Use: "node", Short: "Commands that run on a node (the machine hosting workloads)"}
	c.AddCommand(newNodeServeCmd(), newNodeDialStdioCmd(), newNodeInitCmd())
	return c
}

// ---- dol node serve ----------------------------------------------------------

// serveDeps lets tests replace the docker and kind binaries.
type serveDeps struct {
	docker *dockerx.Docker
	kind   kindx.Runner
}

func newNodeServeCmd() *cobra.Command {
	var cfgPath string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the node agent daemon (unix socket API, admission control, reaper)",
		Long: "Run the node agent. It serves the node API on the unix socket from node.yaml, admits or refuses\n" +
			"leases based on real capacity, and reaps resources of leases whose heartbeats stopped.\n" +
			"Logs are JSON on stderr (set DOL_LOG_LEVEL=debug for request logs). Stops cleanly on SIGTERM.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runNodeServe(cmd.Context(), cfgPath, os.Stderr, serveDeps{})
		},
	}
	c.Flags().StringVar(&cfgPath, "config", config.NodeConfigPath, "node config file")
	return c
}

func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(os.Getenv("DOL_LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return l
}

func runNodeServe(ctx context.Context, cfgPath string, logOut io.Writer, deps serveDeps) error {
	log := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: logLevel()}))
	cfg, err := config.LoadNode(cfgPath)
	if err != nil {
		return err
	}
	docker := deps.docker
	if docker == nil {
		docker = dockerx.New("")
	}
	prov, err := capacity.New(cfg.Capacity, func(ctx context.Context) (string, error) {
		info, err := docker.Info(ctx)
		if err != nil {
			return "", err
		}
		return info.DockerRootDir, nil
	})
	if err != nil {
		return fmt.Errorf("capacity provider: %w", err)
	}
	store, err := node.OpenStore(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	srv, err := node.New(ctx, node.Options{Config: cfg, Store: store, Provider: prov, Docker: docker, Kind: deps.kind, Logger: log})
	if err != nil {
		return err
	}
	ictx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if info, err := docker.Info(ictx); err != nil {
		log.Warn("docker is not reachable; leases will be admitted but workloads will fail until it is", "err", err)
	} else {
		log.Info("docker reachable", "server_version", info.ServerVersion, "data_root", info.DockerRootDir, "arch", info.GoArch())
	}
	cancel()

	ln, err := node.ListenUnix(cfg.Socket, log)
	if err != nil {
		return err
	}
	log.Info("node agent listening", "node", cfg.Name, "socket", cfg.Socket, "version", version.String(),
		"provider", prov.Name(), "store", store.Path(), "draining", srv.Draining())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv.RunReaper(ctx)
	}()
	err = srv.Serve(ctx, ln)
	wg.Wait()
	log.Info("node agent stopped")
	return err
}

// ---- dol node dial-stdio ---------------------------------------------------

func newNodeDialStdioCmd() *cobra.Command {
	var socket, cfgPath string
	c := &cobra.Command{
		Use:   "dial-stdio",
		Short: "Bridge stdin/stdout to the node agent socket (run by clients over ssh)",
		Long: "Bridge stdin/stdout to the node agent's unix socket, like `docker system dial-stdio`.\n" +
			"Clients run `ssh <node> dol node dial-stdio` and speak HTTP over the pipe. Nothing else is\n" +
			"ever written to stdout; errors go to stderr.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if socket == "" {
				socket = socketFromConfig(cfgPath)
			}
			return node.DialStdio(cmd.Context(), socket, os.Stdin, os.Stdout)
		},
	}
	c.Flags().StringVar(&socket, "socket", "", "node agent socket (default: `socket` from the node config)")
	c.Flags().StringVar(&cfgPath, "config", config.NodeConfigPath, "node config file used to find the socket")
	return c
}

// socketFromConfig reads the socket path from the node config, falling back
// to the default when the config is missing or unreadable (dial-stdio must
// work for members of the dol group even if node.yaml is root-only).
func socketFromConfig(path string) string {
	if cfg, err := config.LoadNode(path); err == nil {
		return cfg.Socket
	}
	return config.DefaultNode().Socket
}

// ---- dol node init -------------------------------------------------------------

type nodeInitFlags struct {
	path, name, socket, dataDir, syncRoot, provider, accessMode, kindBinary, lanAddress string
	reserveGB, minDiskGB                                                                float64
	force, dryRun                                                                       bool
}

// initProbes are the environment checks of `dol node init` (fakes in tests).
type initProbes struct {
	dockerInfo func(ctx context.Context) (*dockerx.Info, error)
	kindPath   func(configured string) (string, error)
	chownGroup func(path string) error // give a directory to group dol, if it exists
}

func defaultProbes() initProbes {
	return initProbes{
		dockerInfo: func(ctx context.Context) (*dockerx.Info, error) { return dockerx.New("").Info(ctx) },
		kindPath:   findKind,
		chownGroup: chownToDolGroup,
	}
}

func newNodeInitCmd() *cobra.Command {
	var f nodeInitFlags
	c := &cobra.Command{
		Use:   "init",
		Short: "Write the node config, create data directories and check Docker and kind",
		Long: "Write " + config.NodeConfigPath + " with defaults (overridable by flags), create data_dir and\n" +
			"sync_root, and check that Docker is reachable and kind is installed. Safe to re-run: an existing\n" +
			"config is kept unless --force is given. Use --dry-run to see what would change.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runNodeInit(cmd.Context(), f, cmd.Flags().Changed, cmd.OutOrStdout(), defaultProbes())
		},
	}
	d := config.DefaultNode()
	fl := c.Flags()
	fl.StringVar(&f.path, "config", config.NodeConfigPath, "where to write the node config")
	fl.StringVar(&f.name, "name", shortHostname(d.Name), "node name shown to clients")
	fl.StringVar(&f.socket, "socket", d.Socket, "node API unix socket")
	fl.StringVar(&f.dataDir, "data-dir", d.DataDir, "lease database and kind work files")
	fl.StringVar(&f.syncRoot, "sync-root", d.SyncRoot, "workspace sync root (must match the client's sync.remote_root)")
	fl.StringVar(&f.provider, "provider", d.Capacity.Provider, "capacity provider: auto, linux, wsl2")
	fl.Float64Var(&f.reserveGB, "reserve-memory-gb", d.Capacity.ReserveMemoryGB, "memory never given to leases")
	fl.Float64Var(&f.minDiskGB, "min-free-disk-gb", d.Capacity.MinFreeDiskGB, "refuse new leases below this free disk")
	fl.StringVar(&f.accessMode, "access-mode", d.Kind.AccessMode, "kind API server access: tunnel or lan")
	fl.StringVar(&f.kindBinary, "kind-binary", d.Kind.Binary, "path of the kind binary")
	fl.StringVar(&f.lanAddress, "lan-address", "", "LAN address for lan-mode kind certificates (default: detected)")
	fl.BoolVar(&f.force, "force", false, "overwrite an existing config with defaults and flags")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print what would be done without changing anything")
	return c
}

// shortHostname drops a domain suffix (e.g. "tower.local" → "tower") so the
// default satisfies the node name rule.
func shortHostname(h string) string {
	h, _, _ = strings.Cut(h, ".")
	return h
}

func (f nodeInitFlags) apply(c *config.NodeConfig) {
	c.Name, c.Socket, c.DataDir, c.SyncRoot = f.name, f.socket, f.dataDir, f.syncRoot
	c.Capacity.Provider = f.provider
	c.Capacity.ReserveMemoryGB, c.Capacity.MinFreeDiskGB = f.reserveGB, f.minDiskGB
	c.Kind.AccessMode, c.Kind.Binary, c.Kind.LANAddress = f.accessMode, f.kindBinary, f.lanAddress
}

const nodeConfigHeader = "# docker-offload-lan node config (see docs/IMPLEMENTATION_BRIEF.md §7.1,\n" +
	"# schema: schemas/node-config.schema.json). Written by `dol node init`;\n" +
	"# after editing, restart the agent: sudo systemctl restart dol-node\n"

func renderNodeConfig(c *config.NodeConfig) ([]byte, error) {
	var b strings.Builder
	b.WriteString(nodeConfigHeader)
	enc := yamlEncoder(&b)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func runNodeInit(ctx context.Context, f nodeInitFlags, changed func(string) bool, out io.Writer, p initProbes) error {
	say := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format+"\n", a...) }
	prefix := ""
	if f.dryRun {
		prefix = "[dry-run] "
	}

	// 1. Config file.
	var cfg *config.NodeConfig
	existing, readErr := os.ReadFile(f.path)
	switch {
	case readErr == nil && !f.force:
		c, err := config.LoadNode(f.path)
		if err != nil {
			return fmt.Errorf("the existing config is invalid: %w\n(fix it, or re-run with --force to replace it with defaults)", err)
		}
		cfg = c
		say("config: %s exists and is valid; kept unchanged", f.path)
		for _, name := range []string{"name", "socket", "data-dir", "sync-root", "provider", "reserve-memory-gb",
			"min-free-disk-gb", "access-mode", "kind-binary", "lan-address"} {
			if changed(name) {
				say("  note: --%s ignored because the config exists; pass --force to rewrite it", name)
			}
		}
	case readErr != nil && !errors.Is(readErr, os.ErrNotExist):
		return fmt.Errorf("reading %s: %w (run with sudo?)", f.path, readErr)
	default:
		cfg = config.DefaultNode()
		f.apply(cfg)
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("invalid flags: %w", err)
		}
		data, err := renderNodeConfig(cfg)
		if err != nil {
			return err
		}
		switch {
		case readErr == nil && string(existing) == string(data):
			say("config: %s already up to date", f.path)
		case f.dryRun:
			say("%sconfig: would write %s:\n%s", prefix, f.path, data)
		default:
			if err := writeFileAtomic(f.path, data, 0o644); err != nil {
				return fmt.Errorf("writing %s: %w (run with sudo?)", f.path, err)
			}
			say("config: wrote %s", f.path)
		}
	}

	// 2. Directories.
	for _, d := range []struct {
		path  string
		mode  os.FileMode
		group bool
	}{{cfg.DataDir, 0o750, false}, {cfg.SyncRoot, 0o2775, true}} {
		if fi, err := os.Stat(d.path); err == nil && fi.IsDir() {
			say("dir: %s exists", d.path)
			continue
		} else if err == nil {
			return fmt.Errorf("%s exists but is not a directory", d.path)
		}
		if f.dryRun {
			say("%sdir: would create %s", prefix, d.path)
			continue
		}
		if err := os.MkdirAll(d.path, d.mode.Perm()); err != nil {
			return fmt.Errorf("creating %s: %w (run with sudo?)", d.path, err)
		}
		say("dir: created %s", d.path)
		if d.group && p.chownGroup != nil {
			if err := p.chownGroup(d.path); err != nil {
				say("  warning: %v", err)
			}
		}
	}

	// 3. Environment checks.
	var problems []string
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	info, err := p.dockerInfo(cctx)
	cancel()
	if err != nil {
		say("docker: NOT reachable: %v", err)
		problems = append(problems, "Docker is not reachable: start it (`sudo systemctl start docker`) and make sure the dol user is in the docker group")
	} else {
		say("docker: ok (Engine %s, %s, data-root %s)", info.ServerVersion, info.GoArch(), info.DockerRootDir)
	}
	if kp, err := p.kindPath(cfg.Kind.Binary); err != nil {
		say("kind: not found (%v); kind clusters will fail until it is installed (deploy/linux/install.sh pins it)", err)
	} else {
		say("kind: ok (%s)", kp)
	}
	if len(problems) > 0 {
		return fmt.Errorf("node init incomplete:\n  - %s", strings.Join(problems, "\n  - "))
	}
	if f.dryRun {
		say("%sno changes made", prefix)
	} else {
		say("next: sudo systemctl enable --now dol-node, then run `dol doctor` from your laptop")
	}
	return nil
}
