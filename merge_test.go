package main

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func day(s string) time.Time {
	t, err := time.Parse("20060102", s)
	if err != nil {
		panic(err)
	}
	return t
}

func dailyPiece(v, d string) Piece {
	return Piece{Var: v, Key: d, Out: "/out/.pieces/" + v + "/" + d + ".nc",
		Start: day(d), End: day(d)}
}

func TestResolvePath(t *testing.T) {
	p := dailyPiece("u", "19791101")
	cases := []struct{ path, want string }{
		{"{dataset}_{var}.nc", "era5_u.nc"},
		{"{var}_{year}.nc", "u_1979.nc"},
		{"{yyyymm}/era5_{yyyymmdd}.nc", "197911/era5_19791101.nc"},
		{"{year}/{month}/{day}/{var}.nc", "1979/11/01/u.nc"},
	}
	for _, c := range cases {
		got, err := resolvePath(Layout{Path: c.path}, "era5", p)
		if err != nil || got != c.want {
			t.Errorf("%q: got %q, %v; want %q", c.path, got, err, c.want)
		}
	}

	year := Piece{Var: "tp", Out: "x", Start: day("19000101"), End: day("19001231")}
	if _, err := resolvePath(Layout{Path: "{yyyymmdd}.nc"}, "era20c", year); err == nil {
		t.Error("a day-level path should be refused for a piece holding a year")
	}
	if got, err := resolvePath(Layout{Path: "{var}_{year}.nc"}, "era20c", year); err != nil || got != "tp_1900.nc" {
		t.Errorf("a year-level path for a year piece: got %q, %v", got, err)
	}
}

func TestGroupProducts(t *testing.T) {
	var pieces []Piece
	for _, v := range []string{"u", "v"} {
		for _, d := range []string{"19791101", "19791102"} {
			pieces = append(pieces, dailyPiece(v, d))
		}
	}
	req := &Request{Dataset: "era5", Layouts: map[string]Layout{
		"per_var": {Path: "{dataset}_{var}.nc", Format: "nc4"},
		"per_day": {Path: "{yyyymmdd}.nc", Format: "nc4"},
	}}
	prods, err := groupProducts(req, &Plan{Pieces: pieces}, []string{"per_day", "per_var"}, "/out")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, p := range prods {
		got[strings.TrimPrefix(p.Path, "/out/")] = len(p.Pieces)
	}
	want := map[string]int{"19791101.nc": 2, "19791102.nc": 2, "era5_u.nc": 2, "era5_v.nc": 2}
	if !maps.Equal(got, want) {
		t.Errorf("products %v, want %v", got, want)
	}

	clash := &Request{Dataset: "era5", Layouts: map[string]Layout{
		"a": {Path: "{var}.nc", Format: "nc4"},
		"b": {Path: "{var}.nc", Format: "nc4"},
	}}
	if _, err := groupProducts(clash, &Plan{Pieces: pieces}, []string{"a", "b"}, "/out"); err == nil {
		t.Error("two layouts writing the same file should be refused")
	}
}

func TestLayoutOps(t *testing.T) {
	l := Layout{Rename: map[string]string{"V": "v850", "U": "u850"}, LatAscending: true, Lon180: true}
	got := layoutOps(l)
	want := []string{"-chname,U,u850,V,v850", "-invertlat"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestMergeWithCdo makes small pieces with cdo itself and merges them for real.
func TestMergeWithCdo(t *testing.T) {
	cdo, err := exec.LookPath("cdo")
	if err != nil {
		t.Skip("cdo not on PATH")
	}
	dir := t.TempDir()
	var pieces []Piece
	for _, v := range []string{"u", "v"} {
		for _, d := range []string{"19791101", "19791102", "19791103"} {
			p := dailyPiece(v, d)
			p.Out = filepath.Join(dir, ".pieces", v, d+".nc")
			if err := os.MkdirAll(filepath.Dir(p.Out), 0o755); err != nil {
				t.Fatal(err)
			}
			args := []string{"-O", "-f", "nc4", "-setname," + strings.ToUpper(v),
				"-settaxis," + day(d).Format(dateLayout) + ",00:00:00,1hour",
				"-duplicate,3", "-const,1,r4x2", p.Out}
			if out, err := exec.Command(cdo, args...).CombinedOutput(); err != nil {
				t.Skipf("could not make test files with cdo: %v\n%s", err, out)
			}
			pieces = append(pieces, p)
		}
	}

	req := &Request{Dataset: "era5", Layouts: map[string]Layout{
		"per_var": {Path: "{dataset}_{var}.nc", Format: "nc4"},
		"per_day": {Path: "{yyyymmdd}.nc", Format: "nc", Rename: map[string]string{"U": "u850"}},
	}}
	plan := &Plan{Req: req, Pieces: pieces}
	names := []string{"per_day", "per_var"}

	// A group of 2 forces a staged merge for the three days.
	built, _, err := mergeProducts(context.Background(), cdo, dir, req, plan, names, 2, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if built != 5 {
		t.Errorf("built %d products, want 5 (2 per variable, 3 per day)", built)
	}

	query := func(op, file string) string {
		out, err := exec.Command(cdo, "-s", op, file).CombinedOutput()
		if err != nil {
			t.Fatalf("cdo %s %s: %v\n%s", op, file, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if n := query("ntime", filepath.Join(dir, "era5_u.nc")); n != "9" {
		t.Errorf("era5_u.nc has %s time steps, want 9", n)
	}
	if got := strings.Fields(query("showname", filepath.Join(dir, "19791101.nc"))); !slices.Equal(got, []string{"u850", "V"}) {
		t.Errorf("19791101.nc holds %v, want [u850 V]", got)
	}

	built, skipped, err := mergeProducts(context.Background(), cdo, dir, req, plan, names, 2, 2, false)
	if err != nil || built != 0 || skipped != 5 {
		t.Errorf("rerun: built %d, skipped %d, err %v; want 0, 5, nil", built, skipped, err)
	}
}
