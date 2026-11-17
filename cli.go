package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// OutFlag is shared by every command that works on an output directory.
type OutFlag struct {
	Out string `short:"o" required:"" type:"path" help:"Output directory. Pieces and products all live here."`
}

type ExtractCmd struct {
	Request string `arg:"" type:"existingfile" help:"Request YAML."`
	OutFlag `embed:""`
	Workers int    `default:"0" help:"Parallel cdo calls, each single-threaded. 0 uses jobs from config.toml."`
	Shard   int    `default:"0" help:"This shard's index, for splitting work across PBS array jobs."`
	Of      int    `default:"1" help:"Total number of shards."`
	Cdo     string `help:"cdo binary. Default: cdo from config.toml."`
	Force   bool   `help:"Move aside pieces left by a different request in the output directory."`
	DryRun  bool   `help:"Print the plan and some cdo commands, and fetch nothing."`
	Show    int    `default:"2" help:"cdo commands to print per variable in a dry run."`
	NoMerge bool   `help:"Fetch only; skip building the layout products."`
}

func (c *ExtractCmd) Validate() error {
	if c.Of < 1 {
		return errors.New("--of must be at least 1")
	}
	if c.Shard < 0 || c.Shard >= c.Of {
		return fmt.Errorf("--shard must be in 0..%d", c.Of-1)
	}
	if c.Workers < 0 {
		return errors.New("--workers cannot be negative")
	}
	return nil
}

func printReports(plan *Plan) {
	for _, r := range plan.Reports {
		fmt.Printf("variable     %-4s %s: %d dirs, %d files, %d pieces",
			r.Var, r.Dataset, r.Dirs, r.Files, r.Pieces)
		if n := len(r.Missing); n > 0 {
			shown := r.Missing
			if n > 5 {
				shown = shown[:5]
			}
			fmt.Printf("; %d dates have no file (first: %s)", n, joinDates(shown))
		}
		fmt.Println()
	}
}

func (c *ExtractCmd) Run() error {
	req, cfg, plan, err := loadPlan(c.Request, c.Out)
	if err != nil {
		return err
	}
	cdo := c.Cdo
	if cdo == "" {
		cdo = cfg.Cdo
	}
	workers := c.Workers
	if workers == 0 {
		workers = cfg.Defaults.NumJobs
	}
	mine := shard(plan.Pieces, c.Shard, c.Of)

	dates := req.SourceDates()
	hours := req.HourSet()
	fmt.Printf("dataset      %s\n", req.Dataset)
	fmt.Printf("fingerprint  %s\n", req.Fingerprint())
	fmt.Printf("box          %s (west,east,south,north)\n", plan.Box)
	fmt.Printf("hours        %v (wraps midnight: %t)\n", hours, WrapsMidnight(hours))
	fmt.Printf("source dates %d, %s .. %s\n", len(dates),
		dates[0].Format(dateLayout), dates[len(dates)-1].Format(dateLayout))
	printReports(plan)
	fmt.Printf("pieces       %d total, %d in shard %d of %d\n", len(plan.Pieces), len(mine), c.Shard, c.Of)

	names := make([]string, 0, len(req.Layouts))
	for n := range req.Layouts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		l := req.Layouts[n]
		fmt.Printf("layout       %-10s %s (%s)\n", n, l.Path, l.Format)
	}

	if c.DryRun {
		shown := map[string]int{}
		for _, p := range mine {
			if shown[p.Var] >= c.Show {
				continue
			}
			shown[p.Var]++
			fmt.Printf("  %s %s %s %s\n", cdo, strings.Join(p.Args, " "), p.Source, p.Out)
		}
		return nil
	}

	if len(mine) == 0 {
		return errors.New("nothing to fetch in this shard")
	}
	if _, err := exec.LookPath(cdo); err != nil {
		return fmt.Errorf("cdo not found as %q: %w", cdo, err)
	}
	if err := stamp(c.Out, c.Request, req.Fingerprint(), c.Force); err != nil {
		return err
	}

	// Ctrl-C and a PBS walltime kill both stop the cdo calls cleanly; finished
	// pieces stay, and rerunning the same command carries on.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("running %d pieces with %d workers\n", len(mine), workers)
	res := runPieces(ctx, cdo, c.Out, mine, workers)
	fmt.Printf("done %d, already present %d, failed %d\n", res.Done, res.Skipped, len(res.Failed))
	if ctx.Err() != nil {
		return errors.New("interrupted; rerun the same command to carry on")
	}
	if len(res.Failed) > 0 {
		fmt.Printf("cdo output for each failure is in %s\n", filepath.Join(c.Out, ".pieces", "failed"))
		return fmt.Errorf("%d pieces failed", len(res.Failed))
	}
	if c.Of > 1 {
		fmt.Printf("shard %d of %d finished; once every shard is done, run: rdaextension merge -o %s\n",
			c.Shard, c.Of, c.Out)
		return nil
	}
	if c.NoMerge {
		return nil
	}
	return runMerge(req, cfg, plan, c.Out, nil, workers, cdo, false)
}

type MergeCmd struct {
	OutFlag `embed:""`
	Layout  []string `help:"Only build these layouts, by name from the request YAML. Default: all."`
	Force   bool     `help:"Rebuild products even when they are newer than their pieces."`
	Workers int      `default:"0" help:"Products built at once. 0 uses jobs from config.toml."`
	Cdo     string   `help:"cdo binary. Default: cdo from config.toml."`
}

func (c *MergeCmd) Run() error {
	reqPath := filepath.Join(c.Out, stampDir, "request.yaml")
	if !fileExists(reqPath) {
		return fmt.Errorf("no %s; run extract into %s first", reqPath, c.Out)
	}
	req, cfg, plan, err := loadPlan(reqPath, c.Out)
	if err != nil {
		return err
	}
	return runMerge(req, cfg, plan, c.Out, c.Layout, c.Workers, c.Cdo, c.Force)
}

func runMerge(req *Request, cfg *Config, plan *Plan, out string, chosen []string,
	workers int, cdo string, force bool) error {
	if cdo == "" {
		cdo = cfg.Cdo
	}
	if workers == 0 {
		workers = cfg.Defaults.NumJobs
	}
	names := chosen
	if len(names) == 0 {
		for n := range req.Layouts {
			names = append(names, n)
		}
		sort.Strings(names)
	}
	if len(names) == 0 {
		fmt.Println("the request has no layouts; nothing to merge")
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	built, skipped, err := mergeProducts(ctx, cdo, out, req, plan, names, workers,
		cfg.Defaults.MergeGroup, force)
	fmt.Printf("products: %d built, %d already up to date\n", built, skipped)
	return err
}

type StatusCmd struct {
	OutFlag `embed:""`
}

// Run rebuilds the plan from the request saved in the output directory and
// counts each piece as done, failed, partial (being written, or killed while
// it was), or still to do.
func (c *StatusCmd) Run() error {
	reqPath := filepath.Join(c.Out, stampDir, "request.yaml")
	if !fileExists(reqPath) {
		return fmt.Errorf("no %s; run extract into %s first", reqPath, c.Out)
	}
	req, _, plan, err := loadPlan(reqPath, c.Out)
	if err != nil {
		return err
	}

	type counts struct{ done, failed, partial, todo int }
	per := map[string]*counts{}
	for _, p := range plan.Pieces {
		t := per[p.Var]
		if t == nil {
			t = &counts{}
			per[p.Var] = t
		}
		switch {
		case fileExists(p.Out):
			t.done++
		case fileExists(failLog(c.Out, p)):
			t.failed++
		case fileExists(p.Out + ".partial"):
			t.partial++
		default:
			t.todo++
		}
	}

	fmt.Printf("request      %s (fingerprint %s)\n", reqPath, req.Fingerprint())
	printReports(plan)
	fmt.Printf("%-8s %8s %8s %8s %8s\n", "variable", "done", "failed", "partial", "to do")
	for _, v := range req.Variables {
		if t := per[v]; t != nil {
			fmt.Printf("%-8s %8d %8d %8d %8d\n", v, t.done, t.failed, t.partial, t.todo)
		}
	}
	return nil
}
