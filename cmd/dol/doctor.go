package main

import (
	"fmt"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/config"
	"github.com/fl0wo/docker-offload-lan/internal/doctor"
	"github.com/fl0wo/docker-offload-lan/internal/nodeclient"
	"github.com/fl0wo/docker-offload-lan/internal/sshx"
	"github.com/fl0wo/docker-offload-lan/internal/version"
	"github.com/spf13/cobra"
)

// errChecksFailed makes dol exit non-zero after the report was printed.
type errChecksFailed struct{ n int }

func (e errChecksFailed) Error() string { return fmt.Sprintf("doctor: %d check(s) failed", e.n) }

type doctorFlags struct {
	node       bool
	json       bool
	nodeName   string
	configPath string
	timeout    time.Duration
}

func newDoctorCmd() *cobra.Command {
	var f doctorFlags
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose every link between this machine and its nodes",
		Long: `Runs end-to-end checks and prints a specific fix for every problem.

On the client (default): config, ssh (BatchMode, ControlMaster), the remote
user's docker group, the docker context, the node agent over ssh, versions,
architecture, clock skew, capacity and disk, unlabeled resources, kind/kubectl
and rsync on both ends, and the local Docker used for fallback.

On a node (--node): docker daemon, dol user, dol-node.service, the agent
socket (mode 0660, group dol), sshd on 2222, kind/kubectl versions, disk
(including /mnt/c on WSL) and WSL interop.

Exit status is 1 if any check failed. Check ids are the section names of
docs/troubleshooting.md.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := runDoctor(cmd, f)
			if r.Failed() {
				return errChecksFailed{r.Summary.Fail}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&f.node, "node", false, "check this machine as a node (run on the node)")
	c.Flags().BoolVar(&f.json, "json", false, "print the report as JSON")
	c.Flags().StringVar(&f.nodeName, "node-name", "", "only check this node (client mode)")
	c.Flags().StringVar(&f.configPath, "config", "", "config file (client: default $XDG_CONFIG_HOME/docker-offload-lan/config.yaml; node: "+config.NodeConfigPath+")")
	c.Flags().DurationVar(&f.timeout, "timeout", 0, "per-check timeout (default: per check, 5-30s)")
	return c
}

func runDoctor(cmd *cobra.Command, f doctorFlags) *doctor.Report {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	var p *doctor.Printer
	var progress func(doctor.Result)
	if !f.json {
		p = doctor.NewPrinter(out)
		progress = p.Result
	}
	var r *doctor.Report
	if f.node {
		if p != nil {
			p.Header(fmt.Sprintf("dol doctor --node (dol %s)", version.String()))
		}
		env := &doctor.NodeEnv{
			ConfigPath: f.configPath,
			NewNodeAPI: func(socket string) doctor.NodeAPI { return nodeclient.NewUnix("local", socket) },
		}
		r = doctor.RunNode(ctx, env, doctor.NodeOptions{Progress: progress, Timeout: f.timeout})
	} else {
		if p != nil {
			p.Header(fmt.Sprintf("dol doctor (dol %s)", version.String()))
		}
		r = doctor.RunClient(ctx, clientDoctorEnv(f.configPath, sshx.Options{}, doctor.ExecRunner{}),
			doctor.ClientOptions{NodeName: f.nodeName, Progress: progress, Timeout: f.timeout})
	}
	if f.json {
		_ = r.WriteJSON(out)
	} else {
		p.Summary(r)
	}
	return r
}

// clientDoctorEnv builds the production client environment.
func clientDoctorEnv(configPath string, ssh sshx.Options, run doctor.Runner) *doctor.ClientEnv {
	if configPath == "" {
		configPath = config.ClientConfigPath()
	}
	// A missing file yields defaults with no nodes; the config check says so.
	cfg, err := config.LoadClient(configPath)
	return &doctor.ClientEnv{
		ConfigPath: configPath, Config: cfg, ConfigErr: err, Run: run, SSH: ssh,
		NewNodeAPI: func(n config.NodeRef) doctor.NodeAPI {
			return nodeclient.NewSSH(n.Name, n.SSHHost, ssh)
		},
	}
}
