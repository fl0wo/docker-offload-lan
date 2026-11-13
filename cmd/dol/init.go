package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/clientsetup"
	"github.com/fl0wo/docker-offload-lan/internal/config"
	"github.com/fl0wo/docker-offload-lan/internal/doctor"
	"github.com/fl0wo/docker-offload-lan/internal/sshx"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
	"github.com/spf13/cobra"
)

type initFlags struct {
	name, host, user, identity, jump, jumpIdentity, configPath string
	port                                                       int
	yes, runClaudeAdd, skipDoctor, skipSSH                     bool
}

func newInitCmd() *cobra.Command {
	var f initFlags
	c := &cobra.Command{
		Use:   "init",
		Short: "Set up this machine as a client: add a node, ssh config, docker context, MCP registration",
		Long: `Adds a node to this client. Safe to re-run; every step reports changed/unchanged.

  1. Writes a Host block to ~/.ssh/config.d/docker-offload-lan (Port 2222, User dol,
     ControlMaster) and adds "Include config.d/*" at the top of ~/.ssh/config
     (backing it up first).
  2. Adds the node to the client config ($XDG_CONFIG_HOME/docker-offload-lan/config.yaml).
  3. Creates docker context dol-<name> → ssh://<name>. Your current docker context
     is never changed.
  4. Accepts the node's ssh host key on first contact (accept-new) and runs dol doctor.
  5. Prints (or with --run-claude-add runs) the Claude Code MCP registration.

Non-interactive: dol init --name tower --host 192.168.1.50 --yes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runInit(cmd, f) },
	}
	fl := c.Flags()
	fl.StringVar(&f.name, "name", "", "node name; also the ssh alias and docker context suffix (default tower)")
	fl.StringVar(&f.host, "host", "", "node IP address or DNS name on the LAN")
	fl.StringVar(&f.user, "user", doctor.NodeUser, "ssh user on the node")
	fl.IntVar(&f.port, "port", doctor.SSHPort, "ssh port on the node")
	fl.StringVar(&f.identity, "identity-file", "", "ssh private key for this node (default: your ssh defaults/agent)")
	fl.StringVar(&f.jumpIdentity, "jump-identity-file", "", "ssh private key for the jump host (default: --identity-file)")
	fl.StringVar(&f.jump, "jump", "", "ssh jump host user@host[:port], e.g. dol-jump@192.168.1.153 for a Windows 10 node (then --host localhost)")
	fl.StringVar(&f.configPath, "config", "", "client config file (default $XDG_CONFIG_HOME/docker-offload-lan/config.yaml)")
	fl.BoolVarP(&f.yes, "yes", "y", false, "non-interactive: accept defaults, never prompt")
	fl.BoolVar(&f.runClaudeAdd, "run-claude-add", false, "run `claude mcp add` instead of only printing it")
	fl.BoolVar(&f.skipDoctor, "skip-doctor", false, "do not run dol doctor at the end")
	fl.BoolVar(&f.skipSSH, "skip-ssh-check", false, "do not contact the node (no host key acceptance, no doctor)")
	return c
}

type prompter struct {
	in          *bufio.Reader
	out         io.Writer
	interactive bool
}

func (p *prompter) ask(question, def string) string {
	if !p.interactive {
		return def
	}
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", question, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", question)
	}
	line, _ := p.in.ReadString('\n')
	if line = strings.TrimSpace(line); line != "" {
		return line
	}
	return def
}

func (p *prompter) confirm(question string, def bool) bool {
	d := "y/N"
	if def {
		d = "Y/n"
	}
	a := strings.ToLower(p.ask(question+" ("+d+")", ""))
	if a == "" {
		return def
	}
	return a == "y" || a == "yes"
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func runInit(cmd *cobra.Command, f initFlags) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	p := &prompter{in: bufio.NewReader(cmd.InOrStdin()), out: out, interactive: !f.yes && isTerminal(os.Stdin)}

	if f.name == "" {
		f.name = p.ask("Node name (ssh alias, docker context dol-<name>)", "tower")
	}
	if f.host == "" {
		f.host = p.ask("Node LAN IP or hostname (setup.ps1 printed it)", "")
	}
	if f.host == "" {
		return fmt.Errorf("--host is required: the node's LAN IP or DNS name (deploy/windows/setup.ps1 prints it), e.g. `dol init --name %s --host 192.168.1.50 --yes`", f.name)
	}
	if f.configPath == "" {
		f.configPath = config.ClientConfigPath()
	}
	spec := clientsetup.HostSpec{Alias: f.name, HostName: f.host, User: f.user, Port: f.port, IdentityFile: f.identity, Jump: f.jump, JumpIdentityFile: f.jumpIdentity}
	if err := spec.Validate(); err != nil {
		return err
	}
	node := config.NodeRef{Name: f.name, SSHHost: f.name, DockerContext: "dol-" + f.name, Priority: 100}

	sp, err := clientsetup.DefaultSSHPaths()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Setting up node %s (%s@%s:%d)\n\n", f.name, f.user, f.host, f.port)
	// The docker context goes first: it is the step that can refuse (a
	// foreign context with the same name), and then nothing else is written.
	steps := []func() (clientsetup.Change, error){
		func() (clientsetup.Change, error) {
			return clientsetup.EnsureDockerContext(ctx, doctor.ExecRunner{}, node.DockerContext, node.SSHHost)
		},
		func() (clientsetup.Change, error) { return clientsetup.UpsertHostBlock(sp, spec) },
		func() (clientsetup.Change, error) { return clientsetup.EnsureInclude(sp, time.Now()) },
		func() (clientsetup.Change, error) { return clientsetup.UpsertClientNode(f.configPath, node) },
	}
	for _, s := range steps {
		c, err := s()
		if err != nil {
			return err
		}
		fmt.Fprintln(out, c)
	}

	if !f.skipSSH {
		fmt.Fprintln(out)
		if err := acceptHostKey(ctx, out, node.SSHHost); err != nil {
			fmt.Fprintf(out, "⚠ could not connect yet: %v\n  (dol doctor below shows what is missing)\n", err)
		}
		if !f.skipDoctor {
			fmt.Fprintln(out)
			r := runDoctor(cmd, doctorFlags{nodeName: node.Name, configPath: f.configPath})
			if r.Failed() {
				fmt.Fprintln(out, "\nSome checks failed. Fix them, then run `dol doctor`. Re-running `dol init` is safe.")
			}
		}
	}

	printClaudeStep(ctx, out, p, f.runClaudeAdd)
	suggestLocalMemory(ctx, out)
	return nil
}

// acceptHostKey connects once with StrictHostKeyChecking=accept-new: a new
// host key is recorded, a changed one is still refused.
func acceptHostKey(ctx context.Context, out io.Writer, alias string) error {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	opts := sshx.Options{ExtraArgs: []string{"-o", "StrictHostKeyChecking=accept-new"}, ConnectTimeout: 10 * time.Second}
	start := time.Now()
	if _, err := opts.Run(cctx, alias, "true"); err != nil {
		var e *sshx.Error
		if errors.As(err, &e) {
			if h := sshx.Hint(e.Stderr); h != "" {
				return fmt.Errorf("%w\n  hint: %s", err, h)
			}
		}
		return err
	}
	fmt.Fprintf(out, "[ok] ssh %s works (host key known), %s\n", alias, time.Since(start).Round(time.Millisecond))
	return nil
}

func printClaudeStep(ctx context.Context, out io.Writer, p *prompter, run bool) {
	cmdline := "claude " + strings.Join(clientsetup.ClaudeMCPAddArgs, " ")
	fmt.Fprintln(out, "\nRegister the MCP server with Claude Code (all projects):")
	fmt.Fprintf(out, "  %s\n", cmdline)
	if !run && p.interactive {
		run = p.confirm("Run it now?", false)
	}
	if !run {
		return
	}
	if _, err := exec.LookPath("claude"); err != nil {
		fmt.Fprintln(out, "⚠ `claude` is not on PATH; run the command above once Claude Code is installed.")
		return
	}
	r := doctor.ExecRunner{}
	if clientsetup.ClaudeMCPRegistered(ctx, r) {
		fmt.Fprintln(out, "[unchanged] docker-offload-lan is already registered with Claude Code")
		return
	}
	if so, se, err := r.Run(ctx, "claude", clientsetup.ClaudeMCPAddArgs...); err != nil {
		fmt.Fprintf(out, "⚠ claude mcp add failed: %s %s\n", strings.TrimSpace(se), err)
	} else {
		fmt.Fprintf(out, "[changed] %s\n", strings.TrimSpace(so))
	}
}

func suggestLocalMemory(ctx context.Context, out io.Writer) {
	r := doctor.ExecRunner{}
	name, err := doctor.DetectLocalContext(ctx, r, "auto")
	if err != nil {
		return
	}
	mem := clientsetup.LocalDockerMemory(ctx, r, name)
	if mem <= 4*v1.GiB {
		return
	}
	fmt.Fprintf(out, "\nTip: your local Docker (%s) has %s. Once remote placement works (`dol doctor` all green),\n"+
		"lower it to ~4 GiB (Docker Desktop: Settings → Resources; OrbStack: Settings → System;\n"+
		"Colima: `colima start --memory 4`) so the laptop keeps its RAM; local fallback still works.\n",
		name, v1.FormatBytes(mem))
}
