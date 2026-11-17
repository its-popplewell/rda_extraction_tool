package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildPlanERA5(t *testing.T) {
	root := t.TempDir()
	for _, day := range []string{"20201101", "20201102", "20201103", "20201104"} {
		dir := filepath.Join(root, "e5.oper.an.pl", day[:6])
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, code := range []string{"128_131_u.ll025uv", "128_132_v.ll025uv"} {
			name := fmt.Sprintf("e5.oper.an.pl.%s.%s00_%s23.nc", code, day, day)
			if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	cfg := &Config{
		Cdo:      "cdo",
		Archives: map[string]string{"era5": root},
		Layouts:  map[string][]string{"rda_month": {"{root}/{stream}/{year}{month}"}},
	}
	datasets := map[string]Dataset{}
	for v, code := range map[string]string{"u": "128_131_u", "v": "128_132_v"} {
		datasets["era5_"+v] = Dataset{
			Archive: "era5", Stream: "e5.oper.an.pl", Layout: "rda_month",
			FilePattern: code + `\.ll025uv\.(?P<key>(?P<year>\d{4})(?P<month>\d{2})(?P<day>\d{2}))00_`,
			Ops:         []string{"-selhour,{hours}", "-sellevel,{levels}", "-sellonlatbox,{box}"},
		}
	}

	req := mustParse(t, baseYAML(wrapHours))
	plan, err := buildPlan(req, cfg, datasets, "/out")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Pieces) != 8 {
		t.Fatalf("got %d pieces, want 8 (4 dates x 2 variables)", len(plan.Pieces))
	}

	p := plan.Pieces[0]
	args := strings.Join(p.Args, " ")
	for _, want := range []string{"-selhour,0,1,2,3,4,5,6,7,8,9,23", "-sellevel,850", "-sellonlatbox,-85,-30,-50,15"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	if want := filepath.Join("/out", ".pieces", "u", "202011", "u_20201101.nc"); p.Out != want {
		t.Errorf("first piece goes to %s, want %s", p.Out, want)
	}
	for _, r := range plan.Reports {
		if len(r.Missing) != 0 {
			t.Errorf("%s: %d dates reported missing, want 0", r.Var, len(r.Missing))
		}
	}
}

func TestHoursWithoutAnHoursOpIsAnError(t *testing.T) {
	req := mustParse(t, baseYAML(wrapHours))
	if _, err := fillOps("era20c_tp", []string{"-selhour,6", "-sellonlatbox,1,2,3,4"}, req); err == nil {
		t.Error("a request with hours should be refused by a dataset that cannot select hours")
	}
}
