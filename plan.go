package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SourceFile is one archive file and the days it covers.
type SourceFile struct {
	Path       string
	Key        string
	Start, End time.Time // first and last day covered, inclusive
}

// Daily is true when the file holds one day, so its piece needs no date selection.
func (f SourceFile) Daily() bool { return f.Start.Equal(f.End) }

// Piece is one cdo call: one source file cut down to the request.
type Piece struct {
	Dataset string   // dataset entry, e.g. era5_u
	Var     string   // the request's variable name, e.g. u
	Key     string   // the file's key group
	Source  string   // archive file
	Out     string   // final path under <out>/.pieces
	Args    []string // cdo arguments, without the input and output files
	Start	time.Time 
	End		time.Time
}

// VarReport is what discovery found for one variable.
type VarReport struct {
	Var, Dataset string
	Dirs         int         // candidate directories that exist
	Files        int         // files in them matching the pattern
	Pieces       int         // files the request needs
	Missing      []time.Time // source dates that no file covers
}

type Plan struct {
	Req     *Request
	Box     string
	Pieces  []Piece
	Reports []VarReport
}

func dayKey(t time.Time) int { return t.Year()*10000 + int(t.Month())*100 + t.Day() }

func joinInts(xs []int) string {
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = strconv.Itoa(x)
	}
	return strings.Join(s, ",")
}

func joinDates(ds []time.Time) string {
	s := make([]string, len(ds))
	for i, d := range ds {
		s[i] = d.Format(dateLayout)
	}
	return strings.Join(s, ", ")
}

// group returns the text a named group matched, or "" if the pattern has no
// such group or it did not take part in the match.
func group(re *regexp.Regexp, m []string, name string) string {
	if i := re.SubexpIndex(name); i >= 0 && i < len(m) {
		return m[i]
	}
	return ""
}

// coverage reads the date groups. year, month and day give one day; year and
// month give a month; year alone is taken to be a whole year.
func coverage(r *Resolved, m []string, path string) (SourceFile, error) {
	num := func(name string) (int, bool, error) {
		s := group(r.Pattern, m, name)
		if s == "" {
			return 0, false, nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, false, fmt.Errorf("%s: group %s=%q is not a number", path, name, s)
		}
		return n, true, nil
	}
	year, _, err := num("year")
	if err != nil {
		return SourceFile{}, err
	}
	month, hasMonth, err := num("month")
	if err != nil {
		return SourceFile{}, err
	}
	day, hasDay, err := num("day")
	if err != nil {
		return SourceFile{}, err
	}

	f := SourceFile{Path: path, Key: group(r.Pattern, m, "key")}
	switch {
	case hasMonth && hasDay:
		f.Start = time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
		f.End = f.Start
	case hasMonth:
		f.Start = time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
		f.End = f.Start.AddDate(0, 1, -1)
	default:
		f.Start = time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
		f.End = time.Date(year, 12, 31, 0, 0, 0, 0, time.UTC)
	}
	return f, nil
}

// discover lists every candidate directory and keeps the files whose names
// match the dataset's pattern. Directories that do not exist are skipped.
func discover(r *Resolved, years []int) ([]SourceFile, int, error) {
	var files []SourceFile
	seen := map[string]bool{}
	dirs := 0
	for _, dir := range r.SearchDirs(years) {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, dirs, fmt.Errorf("listing %s: %w", dir, err)
		}
		dirs++
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			m := r.Pattern.FindStringSubmatch(e.Name())
			if m == nil {
				continue
			}
			path := filepath.Join(dir, e.Name())
			if seen[path] {
				continue
			}
			seen[path] = true
			f, err := coverage(r, m, path)
			if err != nil {
				return nil, dirs, err
			}
			files = append(files, f)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Start.Before(files[j].Start) })
	return files, dirs, nil
}

// fillOps puts the request's hours and levels into the dataset's cdo operators.
// A request that sets hours or levels the dataset cannot select is an error,
// rather than being silently ignored.
func fillOps(name string, ops []string, req *Request) ([]string, error) {
	usesHours, usesLevels := false, false
	var out []string
	for _, op := range ops {
		if strings.Contains(op, "{hours}") {
			usesHours = true
			if len(req.Hours) == 0 {
				continue // all 24 hours: nothing to select
			}
			op = strings.ReplaceAll(op, "{hours}", joinInts(req.HourSet()))
		}
		if strings.Contains(op, "{levels}") {
			usesLevels = true
			if len(req.Levels.Values) == 0 {
				return nil, fmt.Errorf("dataset %s selects levels, but the request has no levels.values", name)
			}
			op = strings.ReplaceAll(op, "{levels}", joinInts(req.Levels.Values))
		}
		if strings.ContainsAny(op, "{}") {
			return nil, fmt.Errorf("dataset %s: op %q has a token nothing fills", name, op)
		}
		out = append(out, op)
	}
	if len(req.Hours) > 0 && !usesHours {
		return nil, fmt.Errorf("the request sets hours, but dataset %s has no {hours} op (its ops: %v)", name, ops)
	}
	if len(req.Levels.Values) > 0 && !usesLevels {
		return nil, fmt.Errorf("the request sets levels, but dataset %s has no {levels} op (its ops: %v)", name, ops)
	}
	return out, nil
}

// coarseOps cut a file covering a month or a year down to the dates wanted:
// the span from the first to the last needed day, and the request's months.
// A midnight-wrapping window also keeps the month after each requested month,
// since a night's morning can fall on the 1st of the next. That keeps a little
// more than needed, never less.
func coarseOps(req *Request, lo, hi time.Time) []string {
	ops := []string{fmt.Sprintf("-seldate,%sT00:00:00,%sT23:59:59",
		lo.Format(dateLayout), hi.Format(dateLayout))}
	if len(req.Time.Months) > 0 {
		months := slices.Clone([]int(req.Time.Months))
		if WrapsMidnight(req.HourSet()) {
			for _, m := range req.Time.Months {
				months = append(months, m%12+1)
			}
		}
		slices.Sort(months)
		months = slices.Compact(months)
		ops = append(ops, "-selmon,"+joinInts(months))
	}
	return ops
}

// buildPlan turns a request into the pieces to fetch. The request's dataset and
// each variable name join into a dataset entry: era5 + u is era5_u.
func buildPlan(req *Request, cfg *Config, datasets map[string]Dataset, out string) (*Plan, error) {
	dates := req.SourceDates()
	if len(dates) == 0 {
		return nil, errors.New("no dates match the time range, months and days in the request")
	}
	want := map[int]bool{}
	yearSet := map[int]bool{}
	for _, d := range dates {
		want[dayKey(d)] = true
		yearSet[d.Year()] = true
	}
	years := make([]int, 0, len(yearSet))
	for y := range yearSet {
		years = append(years, y)
	}
	sort.Ints(years)

	dom := req.Domain
	box := fmt.Sprintf("%g,%g,%g,%g", *dom.West, *dom.East, *dom.South, *dom.North)
	plan := &Plan{Req: req, Box: box}
	outs := map[string]string{} // piece path -> source, to catch two files mapping to one piece

	var errs []error
	for _, v := range req.Variables {
		name := req.Dataset + "_" + v
		ds, ok := datasets[name]
		if !ok {
			errs = append(errs, fmt.Errorf("variable %q: no entry %q in %s", v, name, GetDatasetsPath()))
			continue
		}
		res, err := Resolve(name, ds, cfg, box)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ops, err := fillOps(name, res.Ops, req)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		files, dirs, err := discover(res, years)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		rep := VarReport{Var: v, Dataset: name, Dirs: dirs, Files: len(files)}
		covered := map[int]bool{}
		for _, f := range files {
			var hit []time.Time
			for day := f.Start; !day.After(f.End); day = day.AddDate(0, 0, 1) {
				if want[dayKey(day)] {
					hit = append(hit, day)
					covered[dayKey(day)] = true
				}
			}
			if len(hit) == 0 {
				continue
			}

			args := []string{"-O", "-f", "nc4"}
			pieceName := res.PiecePrefix + "_" + f.Key
			start, end := f.Start, f.End
			if !f.Daily() {
				// The clip window is in the name, so a longer date range makes new
				// pieces instead of trusting old ones cut to the shorter range.
				start, end = hit[0], hit[len(hit)-1]
				args = append(args, coarseOps(req, start, end)...)
				pieceName += "_" + start.Format("20060102") + "-" + end.Format("20060102")
			}
			args = append(args, ops...)

			sub := ""
			if len(f.Key) >= 6 {
				sub = f.Key[:6]
			}
			outPath := filepath.Join(out, ".pieces", res.PieceDir, sub, pieceName+".nc")
			if prev, dup := outs[outPath]; dup {
				errs = append(errs, fmt.Errorf("%s and %s both map to piece %s; the key group must differ per file", prev, f.Path, outPath))
				continue
			}
			outs[outPath] = f.Path

			plan.Pieces = append(plan.Pieces, Piece{
				Dataset: res.Name, Var: v, Key: f.Key,
				Source: f.Path, Out: outPath, Args: args,
				Start: start, End: end,
			})

			// plan.Pieces = append(plan.Pieces, Piece{
			// 	Dataset: res.Name, Var: v, Key: f.Key,
			// 	Source: f.Path, Out: outPath, Args: args,
			// })
			rep.Pieces++
		}
		for _, d := range dates {
			if !covered[dayKey(d)] {
				rep.Missing = append(rep.Missing, d)
			}
		}
		plan.Reports = append(plan.Reports, rep)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	// A fixed order, so every shard deals out the same list.
	sort.SliceStable(plan.Pieces, func(i, j int) bool {
		a, b := plan.Pieces[i], plan.Pieces[j]
		if a.Var != b.Var {
			return a.Var < b.Var
		}
		return a.Out < b.Out
	})
	return plan, nil
}

// loadPlan reads the request, both config files, and builds the plan.
func loadPlan(reqPath, out string) (*Request, *Config, *Plan, error) {
	req, err := LoadRequest(reqPath)
	if err != nil {
		return nil, nil, nil, err
	}
	cfg, err := LoadConfig()
	if err != nil {
		return nil, nil, nil, err
	}
	datasets, err := LoadDatasets()
	if err != nil {
		return nil, nil, nil, err
	}
	plan, err := buildPlan(req, cfg, datasets, out)
	if err != nil {
		return nil, nil, nil, err
	}
	return req, cfg, plan, nil
}

// shard deals the pieces out round-robin: shard k of n takes every n-th piece.
func shard(pieces []Piece, k, n int) []Piece {
	var out []Piece
	for i := k; i < len(pieces); i += n {
		out = append(out, pieces[i])
	}
	return out
}
