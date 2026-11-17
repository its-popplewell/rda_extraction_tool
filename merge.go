package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"strconv"
	"sync"
)

// product is one output file of a layout and the pieces that go into it.
type product struct {
	Layout string
	Path   string
	Pieces []Piece
}

// resolvePath fills a layout's path for one piece. Date tokens come from the
// first day the piece holds; a path that splits finer than a piece is refused.
func resolvePath(l Layout, dataset string, p Piece) (string, error) {
	tk, _ := layoutTokens(l.Path)
	sameYear := p.Start.Year() == p.End.Year()
	sameMonth := sameYear && p.Start.Month() == p.End.Month()
	switch {
	case (tk["day"] || tk["yyyymmdd"]) && !p.Start.Equal(p.End):
		return "", fmt.Errorf("path %q splits by day, but piece %s holds %s to %s",
			l.Path, p.Out, p.Start.Format(dateLayout), p.End.Format(dateLayout))
	case (tk["month"] || tk["yyyymm"]) && !sameMonth:
		return "", fmt.Errorf("path %q splits by month, but piece %s spans months", l.Path, p.Out)
	case tk["year"] && !sameYear:
		return "", fmt.Errorf("path %q splits by year, but piece %s spans years", l.Path, p.Out)
	}
	return strings.NewReplacer(
		"{dataset}", dataset,
		"{var}", p.Var,
		"{year}", fmt.Sprintf("%04d", p.Start.Year()),
		"{month}", fmt.Sprintf("%02d", int(p.Start.Month())),
		"{day}", fmt.Sprintf("%02d", p.Start.Day()),
		"{yyyymm}", p.Start.Format("200601"),
		"{yyyymmdd}", p.Start.Format("20060102"),
	).Replace(l.Path), nil
}

// layoutOps are the cdo operators for a layout's options. cdo applies a chain
// right to left: longitudes are shifted first, then latitude flipped, then
// variables renamed.
func layoutOps(l Layout) []string {
	var ops []string
	if len(l.Rename) > 0 {
		olds := make([]string, 0, len(l.Rename))
		for old := range l.Rename {
			olds = append(olds, old)
		}
		sort.Strings(olds)
		var pairs []string
		for _, old := range olds {
			pairs = append(pairs, old, l.Rename[old])
		}
		ops = append(ops, "-chname,"+strings.Join(pairs, ","))
	}
	if l.LatAscending {
		ops = append(ops, "-invertlat") // archives here are stored north to south
	}
	return ops
}

// groupProducts assigns every piece to a product in each named layout.
func groupProducts(req *Request, plan *Plan, names []string, out string) ([]product, error) {
	var errs []error
	var prods []product
	owner := map[string]string{} // product path -> layout, to catch two layouts writing one file
	for _, name := range names {
		l, ok := req.Layouts[name]
		if !ok {
			errs = append(errs, fmt.Errorf("the request has no layout %q", name))
			continue
		}
		if l.Compress && l.Format == "nc" {
			errs = append(errs, fmt.Errorf("layout %q: compress needs format nc4", name))
			continue
		}
		byPath := map[string][]Piece{}
		failed := false
		for _, p := range plan.Pieces {
			rel, err := resolvePath(l, req.Dataset, p)
			if err != nil {
				errs = append(errs, fmt.Errorf("layout %q: %w", name, err))
				failed = true
				break
			}
			path := filepath.Join(out, rel)
			byPath[path] = append(byPath[path], p)
		}
		if failed {
			continue
		}
		paths := make([]string, 0, len(byPath))
		for path := range byPath {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			if prev, taken := owner[path]; taken {
				errs = append(errs, fmt.Errorf("layouts %q and %q both write %s", prev, name, path))
				continue
			}
			owner[path] = name
			ps := byPath[path]
			sort.SliceStable(ps, func(i, j int) bool {
				if ps[i].Var != ps[j].Var {
					return ps[i].Var < ps[j].Var
				}
				return ps[i].Start.Before(ps[j].Start)
			})
			prods = append(prods, product{Layout: name, Path: path, Pieces: ps})
		}
	}
	return prods, errors.Join(errs...)
}

func cmdLine(cdo string, args []string) string {
	if len(args) > 12 { // a mergetime over 200 files would bury the error
		args = append(append([]string{}, args[:10]...), fmt.Sprintf("... (%d more)", len(args)-10))
	}
	return cdo + " " + strings.Join(args, " ")
}

func runCdo(ctx context.Context, cdo string, args []string) error {
	out, err := exec.CommandContext(ctx, cdo, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v\n%s", cmdLine(cdo, args), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// mergeTime joins files along time in stages of at most group files, since cdo
// cannot open thousands of inputs at once. Intermediate files are removed as
// soon as the next stage has used them; the pieces themselves are never touched.
func mergeTime(ctx context.Context, cdo string, files []string, stem string, group int) (string, error) {
	if group < 2 {
		group = 2
	}
	temps := map[string]bool{}
	for stage := 0; len(files) > 1; stage++ {
		var next []string
		for i := 0; i < len(files); i += group {
			chunk := files[i:min(i+group, len(files))]
			if len(chunk) == 1 {
				next = append(next, chunk[0])
				continue
			}
			out := fmt.Sprintf("%s_s%d_%04d.nc", stem, stage, i/group)
			args := append([]string{"-O", "-f", "nc4", "mergetime"}, chunk...)
			args = append(args, out)
			if err := runCdo(ctx, cdo, args); err != nil {
				return "", err
			}
			for _, f := range chunk {
				if temps[f] {
					os.Remove(f)
					delete(temps, f)
				}
			}
			temps[out] = true
			next = append(next, out)
		}
		files = next
	}
	return files[0], nil
}

// buildProduct joins each variable's pieces along time, combines the variables,
// applies the layout's options, and writes the product through a .partial file.
func buildProduct(ctx context.Context, cdo, tmpRoot string, prod product, l Layout, group int) error {
	if err := os.MkdirAll(tmpRoot, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(tmpRoot, "product-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	byVar := map[string][]string{}
	var vars []string
	for _, p := range prod.Pieces {
		if _, seen := byVar[p.Var]; !seen {
			vars = append(vars, p.Var)
		}
		byVar[p.Var] = append(byVar[p.Var], p.Out)
	}
	sort.Strings(vars)

	var inputs []string
	for _, v := range vars {
		f, err := mergeTime(ctx, cdo, byVar[v], filepath.Join(tmp, v), group)
		if err != nil {
			return err
		}
		inputs = append(inputs, f)
	}

	chain := layoutOps(l)
	if l.Lon180 {
		grid, changed, err := lon180Grid(ctx, cdo, inputs[0], tmp)
		if err != nil {
			return err
		}
		if changed {
			// Rightmost in the chain, so it applies first, to the grid griddes described.
			chain = append(chain, "-setgrid,"+grid)
		}
	}
	if len(inputs) > 1 {
		chain = append(chain, "-merge")
	} 

	if len(chain) == 0 {
		chain = []string{"copy"}
	}
	args := []string{"-O", "-f", l.Format}
	if l.Compress {
		args = append(args, "-z", "zip_1")
	}
	args = append(args, chain...)
	args = append(args, inputs...)

	if err := os.MkdirAll(filepath.Dir(prod.Path), 0o755); err != nil {
		return err
	}
	partial := prod.Path + ".partial"
	args = append(args, partial)
	if err := runCdo(ctx, cdo, args); err != nil {
		os.Remove(partial)
		return err
	}
	return os.Rename(partial, prod.Path)
}

// lon180Grid writes the input's grid description with the first longitude moved
// into -180..180, for -setgrid. It reports false when the grid already starts
// there. Only regular lon-lat grids, described by xfirst and xinc, are handled.
func lon180Grid(ctx context.Context, cdo, input, dir string) (string, bool, error) {
	out, err := exec.CommandContext(ctx, cdo, "-s", "griddes", input).Output()
	if err != nil {
		return "", false, fmt.Errorf("cdo griddes %s: %w", input, err)
	}
	lines := strings.Split(string(out), "\n")
	sawFirst, changed := false, false
	for i, line := range lines {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "xvals", "xbounds":
			return "", false, fmt.Errorf("%s: longitudes are listed point by point; lon180 handles only regular grids", input)
		case "xfirst":
			sawFirst = true
			x, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil {
				return "", false, fmt.Errorf("%s: cannot read xfirst %q", input, strings.TrimSpace(value))
			}
			if x > 180 {
				lines[i] = fmt.Sprintf("xfirst    = %g", x-360)
				changed = true
			}
		}
	}
	if !sawFirst {
		return "", false, fmt.Errorf("%s: grid description has no xfirst", input)
	}
	if !changed {
		return "", false, nil
	}
	path := filepath.Join(dir, "grid180.txt")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return "", false, err
	}
	return path, true, nil
}

// upToDate is true when the product exists and is newer than every piece in it.
func upToDate(path string, pieces []Piece) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	for _, p := range pieces {
		pi, err := os.Stat(p.Out)
		if err != nil || pi.ModTime().After(fi.ModTime()) {
			return false
		}
	}
	return true
}

// mergeProducts builds the named layouts, several products at a time.
func mergeProducts(ctx context.Context, cdo, out string, req *Request, plan *Plan,
	names []string, workers, group int, force bool) (built, skipped int, err error) {

	notDone := 0
	for _, p := range plan.Pieces {
		if !fileExists(p.Out) {
			notDone++
		}
	}
	if notDone > 0 {
		return 0, 0, fmt.Errorf("%d of %d pieces are not done; finish extract first (status shows which)",
			notDone, len(plan.Pieces))
	}

	prods, err := groupProducts(req, plan, names, out)
	if err != nil {
		return 0, 0, err
	}
	tmpRoot := filepath.Join(out, ".merge-tmp")
	defer os.Remove(tmpRoot) // only succeeds once it is empty

	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, workers))
	for _, prod := range prods {
		if ctx.Err() != nil {
			break
		}
		if !force && upToDate(prod.Path, prod.Pieces) {
			skipped++
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(prod product) {
			defer wg.Done()
			defer func() { <-sem }()
			e := buildProduct(ctx, cdo, tmpRoot, prod, req.Layouts[prod.Layout], group)
			mu.Lock()
			defer mu.Unlock()
			if e != nil {
				errs = append(errs, fmt.Errorf("%s: %w", prod.Path, e))
				return
			}
			built++
			fmt.Printf("  wrote %s (%d pieces)\n", prod.Path, len(prod.Pieces))
		}(prod)
	}
	wg.Wait()
	return built, skipped, errors.Join(errs...)
}
