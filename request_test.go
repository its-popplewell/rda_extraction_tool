package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const wrapHours = "hours: [23, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9]\n"

func baseYAML(hours string) string {
	return `dataset: era5
domain: {north: 15, south: -50, west: -85, east: -30}
time: {start: 2020-11-01, end: 2020-11-03, months: [11]}
` + hours + `variables: [u, v]
levels: {values: [850]}
`
}

func mustParse(t *testing.T, text string) *Request {
	t.Helper()
	r, err := ParseRequest([]byte(text))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return r
}

func TestIntSeq(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []int
	}{
		{"list", "days: [5, 6, 7]", []int{5, 6, 7}},
		{"range includes stop", "days: {start: 5, stop: 8}", []int{5, 6, 7, 8}},
		{"range with step", "days: {start: 1, stop: 10, step: 3}", []int{1, 4, 7, 10}},
		{"descending", "days: {start: 3, stop: 1}", []int{3, 2, 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var v struct {
				Days IntSeq `yaml:"days"`
			}
			if err := yaml.Unmarshal([]byte(c.text), &v); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal([]int(v.Days), c.want) {
				t.Errorf("got %v, want %v", v.Days, c.want)
			}
		})
	}
}

func TestWrapsMidnight(t *testing.T) {
	all := make([]int, 24)
	for h := range all {
		all[h] = h
	}
	cases := []struct {
		name  string
		hours []int
		want  bool
	}{
		{"night window", []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 23}, true},
		{"wraps with fewer hours", []int{0, 22, 23}, true},
		{"daytime only", []int{0, 3, 6}, false},
		{"only 23", []int{23}, false},
		{"all 24 hours", all, false},
	}
	for _, c := range cases {
		if got := WrapsMidnight(c.hours); got != c.want {
			t.Errorf("%s: got %t, want %t", c.name, got, c.want)
		}
	}
}

func dateStrings(ds []time.Time) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Format(dateLayout)
	}
	return out
}

func TestSourceDates(t *testing.T) {
	wrap := mustParse(t, baseYAML(wrapHours))
	got := dateStrings(wrap.SourceDates())
	want := []string{"2020-11-01", "2020-11-02", "2020-11-03", "2020-11-04"}
	if !slices.Equal(got, want) {
		t.Errorf("wrapping hours: got %v, want %v", got, want)
	}

	plain := mustParse(t, baseYAML("hours: [0, 3, 6]\n"))
	got = dateStrings(plain.SourceDates())
	want = []string{"2020-11-01", "2020-11-02", "2020-11-03"}
	if !slices.Equal(got, want) {
		t.Errorf("non-wrapping hours: got %v, want %v", got, want)
	}
}

func TestFingerprint(t *testing.T) {
	a := mustParse(t, baseYAML(wrapHours))
	reordered := strings.NewReplacer(
		"variables: [u, v]", "variables: [v, u]",
		"hours: [23, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9]", "hours: [9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 23, 23]",
	).Replace(baseYAML(wrapHours))
	if a.Fingerprint() != mustParse(t, reordered).Fingerprint() {
		t.Error("order of variables and hours should not change the fingerprint")
	}

	longer := strings.Replace(baseYAML(wrapHours), "end: 2020-11-03", "end: 2021-03-31", 1)
	if a.Fingerprint() != mustParse(t, longer).Fingerprint() {
		t.Error("the date range should not change the fingerprint")
	}

	moved := strings.Replace(baseYAML(wrapHours), "west: -85", "west: -80", 1)
	if a.Fingerprint() == mustParse(t, moved).Fingerprint() {
		t.Error("a different box should change the fingerprint")
	}
}

func TestLayoutValidation(t *testing.T) {
	cases := []struct {
		path    string
		wantErr string // empty means valid
	}{
		{"{dataset}_{var}.nc", ""},
		{"{yyyymm}/era5_{yyyymmdd}.nc", ""},
		{"{var}_{year}.nc", ""},
		{"all.nc", ""},
		{"{var}_{day}.nc", "{day}"},
		{"{month}.nc", "{month}"},
		{"{foo}.nc", "unknown token"},
		{"/abs/{var}.nc", "relative"},
		{"../{var}.nc", "relative"},
		{"{var.nc", "unmatched"},
		{"", "path is empty"},
	}
	for _, c := range cases {
		text := baseYAML(wrapHours) + "layouts:\n  x: \"" + c.path + "\"\n"
		_, err := ParseRequest([]byte(text))
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%q: unexpected error: %v", c.path, err)
		case c.wantErr != "" && err == nil:
			t.Errorf("%q: expected an error mentioning %q", c.path, c.wantErr)
		case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
			t.Errorf("%q: error %q does not mention %q", c.path, err, c.wantErr)
		}
	}
}

func TestUnknownKeyIsAnError(t *testing.T) {
	if _, err := ParseRequest([]byte(baseYAML(wrapHours) + "bogus: 1\n")); err == nil {
		t.Error("a misspelled or unknown key should be rejected")
	}
}
