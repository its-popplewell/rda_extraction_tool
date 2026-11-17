package main

import (
    "bytes"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "errors"
    "fmt"
    "os"
    "path/filepath"
    "regexp"
    "slices"
    "sort"
    "strings"
    "time"

    "gopkg.in/yaml.v3"
)

const dateLayout = "2006-01-02"

var (
    // knownDatasets = []string{"era5", "era20c"}
    layoutTokenNames = []string{"dataset", "var", "year", "month", "day", "yyyymm", "yyyymmdd"}
    tokenRE = regexp.MustCompile(`\{([^{}]*)\}`)
)

// IntSeq: a list or a {start, stop, step} range w stop inclusive
type IntSeq []int

func (s *IntSeq) UnmarshalYAML(n *yaml.Node) error {
    switch n.Kind {
    case yaml.SequenceNode:
        var xs []int
        if err := n.Decode(&xs); err != nil {
            return err
        }
        *s = xs
        return nil

    case yaml.MappingNode:
        var r struct {
            Start *int `yaml:"start"`
            Stop *int `yaml:"stop"`
            Step *int `yaml:"step"`
        }
        if err := n.Decode(&r); err != nil {
            return err
        }

        if r.Start == nil || r.Stop == nil {
            return fmt.Errorf("line %d: a range needs boths a start and a stop", n.Line)
        }

        step := 1
        if *r.Stop < *r.Start {
            step = -1
        }
        if r.Step != nil {
            step = *r.Step
        }
        if step == 0 {
            return fmt.Errorf("line %d: step cannot be 0", n.Line)
        }

        var xs []int
        for v := *r.Start; (step > 0 && v <= *r.Stop) || (step < 0 && v >= *r.Stop); v += step {
            xs = append(xs, v)
        }
        *s = xs

        return nil
    }

    return fmt.Errorf("line %d: expected a list or a {start, stop, step} mapping", n.Line)
}

type Date struct {time.Time}

func (d *Date) UnmarshalYAML(n *yaml.Node) error {
    if n.Kind != yaml.ScalarNode {
        return fmt.Errorf("line %d: expected a date (yyyy-mm-dd)", n.Line)
    }
    t, err := time.Parse(dateLayout, n.Value)
    if err != nil {
        return fmt.Errorf("line %d: %q is not a yyyy-mm-dd date", n.Line, n.Value)
    }

    d.Time = t
    return nil
}

// -- REQUEST PIECES -- 
// NOTE: Pointers so that a missing key is an error and not a silent 0.
type Domain struct {
	North *float64 `yaml:"north" json:"north"`
	South *float64 `yaml:"south" json:"south"`
	West  *float64 `yaml:"west" json:"west"`
	East  *float64 `yaml:"east" json:"east"`
}

type TimeSpec struct {
	Start  Date   `yaml:"start"`
	End    Date   `yaml:"end"`
	Months IntSeq `yaml:"months"`
	Days   IntSeq `yaml:"days"`
}

type Levels struct {
	Values IntSeq `yaml:"values"`
	Units  string `yaml:"units"`
}

// TODO: WHAT THE FUCK
// NOTE: Layout says how pieces are grouped and merged into products. The tokens in
// Path decide the grouping: pieces that resolve to the same path share a file.
type Layout struct {
	Path         string            `yaml:"path"`
	Format       string            `yaml:"format"`
	Compress     bool              `yaml:"compress"`
	Lon180       bool              `yaml:"lon180"`
	LatAscending bool              `yaml:"lat_ascending"`
	Rename       map[string]string `yaml:"rename"`
}

// A bare string is shorthand for a layout with only a path.
func (l *Layout) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		l.Path = n.Value
		return nil
	}
	type plain Layout
	return n.Decode((*plain)(l))
}

type Request struct {
    Dataset   string            `yaml:"dataset"`
	Domain    Domain            `yaml:"domain"`
	Time      TimeSpec          `yaml:"time"`
	Hours     IntSeq            `yaml:"hours"`
	Variables []string          `yaml:"variables"`
	Levels    Levels            `yaml:"levels"`
	Layouts   map[string]Layout `yaml:"layouts"`
}

func LoadRequest(path string) (*Request, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, err
    }

    r, err := ParseRequest(data)
    if err != nil {
        return nil, err
    }

    return r, nil
}

func ParseRequest(data []byte) (*Request, error) {
    dec := yaml.NewDecoder(bytes.NewReader(data))
    dec.KnownFields(true) // a mis-spelled key is an error
    var r Request
    if err := dec.Decode(&r); err != nil {
        return nil, err
    }
    r.applyDefaults()
    if err := r.Validate(); err != nil {
        return nil, err
    }

    return &r, nil
}

func (r *Request) applyDefaults() {
    if r.Levels.Units == "" {
        r.Levels.Units = "hPa"
    }

    for name, l := range r.Layouts {
        if l.Format == "" {
            l.Format = "nc4"
        }

        r.Layouts[name] = l
    }
}

// Validate reports every problem at once, so a request is fixed in one pass.
func (r *Request) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if r.Dataset == "" {
		add("dataset is required, e.g. era5; it is joined with each variable to find the entry in dataset.toml")
	}

	d := r.Domain
	if d.North == nil || d.South == nil || d.West == nil || d.East == nil {
		add("domain needs north, south, west and east")
	} else {
		if *d.South < -90 || *d.South > 90 || *d.North < -90 || *d.North > 90 {
			add("domain latitudes must be in [-90, 90]: south=%g, north=%g", *d.South, *d.North)
		}
		if *d.South >= *d.North {
			add("domain south (%g) must be < north (%g)", *d.South, *d.North)
		}
		if *d.West < -180 || *d.West > 180 || *d.East < -180 || *d.East > 180 {
			add("domain longitudes must be in [-180, 180]: west=%g, east=%g", *d.West, *d.East)
		}
		if *d.West >= *d.East {
			add("domain west (%g) must be < east (%g); no antimeridian wrap yet", *d.West, *d.East)
		}
	}

	t := r.Time
	switch {
	case t.Start.IsZero() || t.End.IsZero():
		add("time needs start and end (yyyy-mm-dd)")
	case t.End.Before(t.Start.Time):
		add("time start (%s) must be on or before end (%s)",
			t.Start.Format(dateLayout), t.End.Format(dateLayout))
	}
	for _, m := range t.Months {
		if m < 1 || m > 12 {
			add("time.months must be in 1..12, got %d", m)
			break
		}
	}
	for _, day := range t.Days {
		if day < 1 || day > 31 {
			add("time.days must be in 1..31, got %d", day)
			break
		}
	}
	for _, h := range r.Hours {
		if h < 0 || h > 23 {
			add("hours must be in 0..23, got %d", h)
			break
		}
	}

	if len(r.Variables) == 0 {
		add("variables is empty")
	}
	seen := map[string]bool{}
	for _, v := range r.Variables {
		if seen[v] {
			add("variable %q is listed twice", v)
		}
		seen[v] = true
	}

	// if len(r.Levels.Values) == 0 {
	// 	add("levels.values is empty")
	// }
	for _, l := range r.Levels.Values {
		if l <= 0 {
			add("levels.values must be positive, got %d", l)
			break
		}
	}
	if r.Levels.Units != "hPa" {
		add("levels.units must be %q, got %q", "hPa", r.Levels.Units)
	}

	names := make([]string, 0, len(r.Layouts))
	for n := range r.Layouts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, p := range r.Layouts[name].problems() {
			add("layout %q: %s", name, p)
		}
	}

	return errors.Join(errs...)
}

// TODO: HUH
func layoutTokens(path string) (tokens map[string]bool, stray bool) {
    tokens = map[string]bool{}
    rest := tokenRE.ReplaceAllStringFunc(path, func(m string) string {
        tokens[m[1:len(m) - 1]] = true
        return ""
    })
    return tokens, strings.ContainsAny(rest, "{}")
}

func (l Layout) problems() []string {
    if l.Path == "" {
        return []string{"path is empty!"}
    }

    var out []string
    if !filepath.IsLocal(l.Path) {
        out = append(out, "path must be relative and stay inside the output directory")
    }

    tk, stray := layoutTokens(l.Path)
    if stray {
        out = append(out, "unmatched { or } in path")
    }

    var unknown []string
    for name := range tk {
        if !slices.Contains(layoutTokenNames, name) {
            unknown = append(unknown, "{"+name+"}")
        }
    }

    if len(unknown) > 0 {
        sort.Strings(unknown)
        out = append(out, fmt.Sprintf("unknown token(s) %s; allowed: {%s}",
            strings.Join(unknown, ", "), strings.Join(layoutTokenNames, "}, {")))
    }

    hasYear := tk["year"] || tk["yyyymm"] || tk["yyyymmdd"]
    hasMonth := tk["month"] || tk["yyyymm"] || tk["yyyymmdd"]
    if tk["month"] && !hasYear {
        out = append(out, "{month} also needs {year} (or use {yyyymm})")
    }
    if tk["day"] && !(hasYear && hasMonth) {
        out = append(out, "{day} also needs {month} and {year} (or use {yyyymmdd})")
    }
    if l.Format != "nc" && l.Format != "nc4" {
        out = append(out, fmt.Sprintf("format must be nc or nc4, got %q", l.Format))
    }

    return out
}

// HourSet is the requested hours, sorted and de-duped. If omitted, assumes 24
func (r *Request) HourSet() []int {
    if len(r.Hours) == 0 {
        all := make([]int, 24)
        for h := range all {
            all[h] = h
        }
        return all
    }

    hours := slices.Clone([]int(r.Hours))
    slices.Sort(hours)
    
    return slices.Compact(hours)
}

// WrapsMidnight is true for a window like 23..09 UTC.
func WrapsMidnight(hours []int) bool {
    return len(hours) < 24 && slices.Contains(hours, 23) && slices.Contains(hours, 0)
}

func qualifies(d time.Time, months, days IntSeq) bool {
    return (len(months) == 0 || slices.Contains(months, int(d.Month()))) &&
        (len(days) == 0 || slices.Contains(days, d.Day()))
}

// SourceDates lists the dates whose files must be fetchedj
func (r *Request) SourceDates() []time.Time {
    doesWrap := WrapsMidnight(r.HourSet())
    key := func(d time.Time) int { return d.Year()*10000 + int(d.Month())*100 + d.Day() }
    want := map[int]time.Time{}

    for d := r.Time.Start.Time; !d.After(r.Time.End.Time); d = d.AddDate(0, 0, 1) {
        if !qualifies(d, r.Time.Months, r.Time.Days) {
            continue
        }

        want[key(d)] = d
        if doesWrap {
            next := d.AddDate(0, 0, 1)
            want[key(next)] = next
        }
    }

    out := make([]time.Time, 0, len(want))
    for _, d := range want {
        out = append(out, d)
    }

    sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
    return out
}

// Fingerprint identifies what a piece holds
func (r *Request) Fingerprint() string {
    vars := slices.Clone(r.Variables)
    sort.Strings(vars)
    levels := slices.Clone([]int(r.Levels.Values))
    sort.Ints(levels)
    canon := struct {
        Dataset     string      `json:"dataset"`
        Domain      Domain      `json:"domain"`
        Hours       []int       `json:"hours"`
        Variables   []string    `json:"variables"`
        LevelsHPa   []int       `json:"levels_hpa"`
    }{r.Dataset, r.Domain, r.HourSet(), vars, levels}

    b, _ := json.Marshal(canon)
    sum := sha256.Sum256(b)
    return hex.EncodeToString(sum[:8])
}
