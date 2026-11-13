// Package doctor implements `dol doctor`: end-to-end diagnostics of every
// link between the client and its nodes (client mode) or of the node itself
// (`--node`). Checks are a table of functions over an environment whose
// side effects (processes, files, the node API) are injectable, so every
// check is unit-tested with fakes (ADR-0033).
package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Status is the outcome class of a check.
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
	StatusInfo Status = "info" // informational, never a problem
	StatusSkip Status = "skip" // not run because a prerequisite failed or n/a
)

// Outcome is what a check function returns.
type Outcome struct {
	Status  Status
	Message string
	// Fix is a specific, actionable remedy (empty for ok/info).
	Fix string
}

func ok(format string, a ...any) Outcome {
	return Outcome{Status: StatusOK, Message: fmt.Sprintf(format, a...)}
}
func info(format string, a ...any) Outcome {
	return Outcome{Status: StatusInfo, Message: fmt.Sprintf(format, a...)}
}
func skip(format string, a ...any) Outcome {
	return Outcome{Status: StatusSkip, Message: fmt.Sprintf(format, a...)}
}
func warn(fix, format string, a ...any) Outcome {
	return Outcome{Status: StatusWarn, Message: fmt.Sprintf(format, a...), Fix: fix}
}
func fail(fix, format string, a ...any) Outcome {
	return Outcome{Status: StatusFail, Message: fmt.Sprintf(format, a...), Fix: fix}
}

// Result is one executed check.
type Result struct {
	ID         string        `json:"id"`
	Title      string        `json:"title"`
	Node       string        `json:"node,omitempty"`
	Status     Status        `json:"status"`
	Message    string        `json:"message"`
	Fix        string        `json:"fix,omitempty"`
	DurationMS int64         `json:"duration_ms"`
	Duration   time.Duration `json:"-"`
}

// Summary counts results per status.
type Summary struct {
	OK   int `json:"ok"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
	Info int `json:"info"`
	Skip int `json:"skip"`
}

// Report is the full output of a doctor run.
type Report struct {
	Mode    string   `json:"mode"` // "client" | "node"
	Results []Result `json:"results"`
	Summary Summary  `json:"summary"`
}

// Failed reports whether any check failed.
func (r *Report) Failed() bool { return r.Summary.Fail > 0 }

func (r *Report) add(res Result) {
	r.Results = append(r.Results, res)
	switch res.Status {
	case StatusOK:
		r.Summary.OK++
	case StatusWarn:
		r.Summary.Warn++
	case StatusFail:
		r.Summary.Fail++
	case StatusInfo:
		r.Summary.Info++
	case StatusSkip:
		r.Summary.Skip++
	}
}

// check is one row of a check table. S is the per-run state shared between
// checks of the same table (so later checks can skip when earlier ones fail).
type check[S any] struct {
	ID      string
	Title   string
	Timeout time.Duration // 0 = defaultCheckTimeout
	Run     func(ctx context.Context, s S) Outcome
}

const defaultCheckTimeout = 15 * time.Second

func runTable[S any](ctx context.Context, r *Report, node string, s S, table []check[S], progress func(Result)) {
	for _, c := range table {
		to := c.Timeout
		if to == 0 {
			to = defaultCheckTimeout
		}
		cctx, cancel := context.WithTimeout(ctx, to)
		start := time.Now()
		out := safeRun(cctx, c, s)
		if out.Status == StatusFail && cctx.Err() != nil && ctx.Err() == nil && !strings.Contains(out.Message, "timed out") {
			out.Message += fmt.Sprintf(" (timed out after %s)", to)
		}
		cancel()
		d := time.Since(start)
		res := Result{ID: c.ID, Title: c.Title, Node: node, Status: out.Status, Message: out.Message, Fix: out.Fix,
			Duration: d, DurationMS: d.Milliseconds()}
		r.add(res)
		if progress != nil {
			progress(res)
		}
	}
}

func safeRun[S any](ctx context.Context, c check[S], s S) (out Outcome) {
	defer func() {
		if p := recover(); p != nil {
			out = fail("this is a bug in dol; please report it", "check panicked: %v", p)
		}
	}()
	return c.Run(ctx, s)
}

// Runner runs a local process. Tests substitute a fake.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)
}

// ExecRunner runs real processes.
type ExecRunner struct {
	// Env is appended to the current environment (e.g. DOCKER_CONFIG, PATH).
	Env []string
}

// Run implements Runner.
func (r ExecRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(r.Env) > 0 {
		cmd.Env = append(os.Environ(), r.Env...)
	}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("%w (timed out)", ctx.Err())
	}
	return so.String(), se.String(), err
}

// isNotFound reports whether err means the executable does not exist.
func isNotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}

// firstLine returns the first non-empty line of s, trimmed.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// errText combines err and stderr into one short line.
func errText(err error, stderr string) string {
	se := strings.TrimSpace(stderr)
	if se != "" {
		lines := strings.Split(se, "\n")
		if len(lines) > 2 {
			lines = lines[len(lines)-2:]
		}
		return strings.Join(lines, " | ")
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// WriteJSON writes the report as indented JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Printer renders results for humans, one line per check.
type Printer struct {
	W     io.Writer
	Color bool
	node  string
	first bool
}

// NewPrinter returns a Printer; color is enabled for terminals unless
// NO_COLOR is set.
func NewPrinter(w io.Writer) *Printer {
	color := false
	if f, ok := w.(*os.File); ok && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			color = true
		}
	}
	return &Printer{W: w, Color: color, first: true}
}

func (p *Printer) paint(code, s string) string {
	if !p.Color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// Symbol returns the status glyph.
func Symbol(s Status) string {
	switch s {
	case StatusOK:
		return "✓"
	case StatusWarn:
		return "⚠"
	case StatusFail:
		return "✗"
	case StatusInfo:
		return "i"
	}
	return "·"
}

func (p *Printer) color(s Status) string {
	switch s {
	case StatusOK:
		return "32"
	case StatusWarn:
		return "33"
	case StatusFail:
		return "31"
	case StatusInfo:
		return "36"
	}
	return "2"
}

// Header prints a section header.
func (p *Printer) Header(title string) {
	if !p.first {
		fmt.Fprintln(p.W)
	}
	p.first = false
	fmt.Fprintln(p.W, p.paint("1", title))
}

// Result prints one result (and a node header when the node changes).
func (p *Printer) Result(r Result) {
	if r.Node != p.node {
		p.node = r.Node
		if r.Node != "" {
			p.Header("Node " + r.Node)
		}
	}
	dur := ""
	if r.Duration >= time.Millisecond {
		dur = p.paint("2", " ("+fmtDuration(r.Duration)+")")
	}
	fmt.Fprintf(p.W, "  %s %-20s %s%s\n", p.paint(p.color(r.Status), Symbol(r.Status)), r.ID, r.Message, dur)
	if r.Fix != "" && (r.Status == StatusFail || r.Status == StatusWarn) {
		fmt.Fprintf(p.W, "    %s %s\n", p.paint("2", "fix:"), r.Fix)
	}
}

// Summary prints the final line.
func (p *Printer) Summary(r *Report) {
	s := r.Summary
	fmt.Fprintln(p.W)
	parts := []string{p.paint("32", fmt.Sprintf("%d ok", s.OK))}
	if s.Warn > 0 {
		parts = append(parts, p.paint("33", fmt.Sprintf("%d warning(s)", s.Warn)))
	}
	if s.Fail > 0 {
		parts = append(parts, p.paint("31", fmt.Sprintf("%d failed", s.Fail)))
	}
	if s.Info > 0 {
		parts = append(parts, fmt.Sprintf("%d info", s.Info))
	}
	if s.Skip > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", s.Skip))
	}
	fmt.Fprintf(p.W, "Summary: %s\n", strings.Join(parts, ", "))
	if s.Fail > 0 {
		fmt.Fprintln(p.W, "See docs/troubleshooting.md (sections are named after the check ids).")
	}
}

func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
}
