package main

import (
    "regexp";
    "path/filepath";
    "os";
    "fmt";
    "strings";
    "sort";
    "strconv";

    "github.com/BurntSushi/toml"
)

// `Config` is the machine side -- the stuff that shouldn't change much,
// unless new datasets are getting added
type Config struct {
    Cdo         string                  `toml:"cdo"`
    Defaults    Defaults                `toml:"defaults"`
    Archives    map[string]string       `toml:"archives"`
    Boxes       map[string]string       `toml:"boxes"`
    Layouts     map[string][]string     `toml:"layouts"`
}

type Defaults struct {
    NumJobs     int     `toml:"jobs"`
    MergeGroup  int     `toml:"merge_group"`
    Box         string  `toml:"box"`
}

// D
type Dataset struct {
    Description     string      `toml:"description"`
    Archive         string      `toml:"archive"`
    Stream          string      `toml:"stream"`
    Case            string      `toml:"case"`
    Variable        string      `toml:"variable"`
    Layout          any         `toml:"layout"`
    FilePattern     string      `toml:"file_pattern"`
    Ops             []string    `toml:"ops"`

    // All three of the follow come from the dataset name, so typically
    // one can omit these on creation
    PieceDir        string      `toml:"piece_dir"`
    PiecePrefix     string      `toml:"piece_prefix"`
    MergedName      string      `toml:"merged_name"`
}

type DatasetFile struct {
    Datasets map [string]Dataset `toml:"datasets"`
}

type Resolved struct {
    Name            string
    Description     string
    Root            string
    Stream          string
    Case            string
    Variable        string
    Templates       []string
    Pattern         *regexp.Regexp
    Ops             []string
    PieceDir        string
    PiecePrefix     string
    MergedName      string
}

var templateVars = []string{
    "root", "stream", "case", "variable", "year", "month",
    "decade0", "decade9",
}

func getConfigDirPath() string {
    dir := os.Getenv("XDG_CONFIG_HOME")
    if dir == "" {
        home, err := os.UserHomeDir()
        if err != nil {
            return "."
        }
        dir = filepath.Join(home, ".config")
    }

    return filepath.Join(dir, "rda_extract")
}

func GetConfigPath() string { return filepath.Join(getConfigDirPath(), "config.toml") }
func GetDatasetsPath() string {
    dir := getConfigDirPath()
    for _, name := range []string{"datasets.toml", "dataset.toml"} {
        p := filepath.Join(dir, name)
        if _, err := os.Stat(p); err == nil {
            return p
        }
    }
    return filepath.Join(dir, "datasets.toml")
}
// Load the config TOML file into a Config object
func LoadConfig() (*Config, error) {
    var cfg Config
    metadata, err := toml.DecodeFile(GetConfigPath(), &cfg)
    if err != nil {
        return nil, fmt.Errorf("reading %s: %w", GetConfigPath(), err)
    }

    if undecodedMd := metadata.Undecoded(); len(undecodedMd) > 0 {
        keys := make([]string, len(undecodedMd))
        for i, k := range undecodedMd {
            keys[i] = k.String()
        }

        return nil, fmt.Errorf("%s: unknown keys: %s",
            GetConfigPath(), strings.Join(keys, ", "))
    }

    if cfg.Cdo == "" {
        cfg.Cdo = "cdo"
    }

    if cfg.Defaults.NumJobs == 0 {
        cfg.Defaults.NumJobs = 8
    }

    // WHAT IS THIS
    if cfg.Defaults.MergeGroup == 0 {
        cfg.Defaults.MergeGroup = 200
    }

    return &cfg, nil
}

// Loads the dataset TOML file into a dictionary which maps
// the dataset name to a dataset object
func LoadDatasets() (map[string]Dataset, error) {
    var df DatasetFile
    metadata, err := toml.DecodeFile(GetDatasetsPath(), &df)
    if err != nil {
        return nil, fmt.Errorf("reading %s: %w", GetDatasetsPath(), err)
    }

    if undecodedMd := metadata.Undecoded(); len(undecodedMd) > 0 {
        keys := make([]string, len(undecodedMd))
        for i, k := range undecodedMd {
            keys[i] = k.String()
        }
        return nil, fmt.Errorf("%s: unknown keys: %s",
            GetDatasetsPath(), strings.Join(keys, ", "))
    }

    return df.Datasets, nil
}

// `parseToLayoutTemplates` turns an input (from TOML) into a list
// of string
func parseToLayoutTemplates(l any, layouts map[string][]string) ([]string, error) {
    switch v:= l.(type) {
    case nil:
        return nil, fmt.Errorf("no layout given")
    case string:
        t, ok := layouts[v]
        if !ok {
            // Q: WHY THREE ARGUMENTS? A: i think it is allocates
            names := make([]string, 0, len(layouts))
            for n := range layouts {
                names = append(names, n)
            }
            sort.Strings(names)

            return nil, fmt.Errorf("unknown layout %q; config has: %s",
                v, strings.Join(names, ", "))
        }
        return t, nil
    case []any:
        // TOML gives []any for an inline array, so each element needs a
        // type check
        out := make([]string, 0, len(v))
        for i, e := range v {
            s, ok := e.(string)
            if !ok {
                return nil, fmt.Errorf("layout element %d is %T, want string",
                    i, e)
            }
            out = append(out, s)
        }

        return out, nil
    default:
        return nil, fmt.Errorf("layout is %T, want a name or a list of templates", l)
    }
}

// WHAT IS THIS BLACK MAGIC
var placeholderRe = regexp.MustCompile(`\{(\w+)\}`)

// Checks a template and rejects if it is using a variable not
// specified in the file
func checkTemplate(t string) error {
    known := map[string]bool{}
    for _, v := range templateVars {
        known[v] = true
    }

    for _, m := range placeholderRe.FindAllStringSubmatch(t, -1) {
        if !known[m[1]] {
            return fmt.Errorf("template %q uses unknown variable {%s}; known: %s",
                t, m[1], strings.Join(templateVars, ", "))
        }
    }

    return nil
}

// Resolve looks up an archive, the layout and the box, fills the defaults,
// and compiles the pattern.
func Resolve (name string, d Dataset, cfg *Config, box string) (*Resolved, error) {
    root, ok := cfg.Archives[d.Archive]
    if !ok {
        names := make([]string, 0, len(cfg.Archives))
        for n := range cfg.Archives {
            names = append(names, n)
        }
        sort.Strings(names)
        return nil, fmt.Errorf("dataset %s: unknown archive %q; config has %s",
            name, d.Archive, strings.Join(names, ", "))
    }

    templates, err := parseToLayoutTemplates(d.Layout, cfg.Layouts) 
    if err != nil {
        return nil, fmt.Errorf("dataset %s: %w", name, err)
    }
    for _, t := range templates {
        if err := checkTemplate(t); err != nil {
            return nil, fmt.Errorf("dataset %s: %w", name, err)
        }
    }

    if d.FilePattern == "" {
        return nil, fmt.Errorf("dataset %s: file_pattern is required", name)
    }
    re, err := regexp.Compile(d.FilePattern)
    if err != nil {
        return nil, fmt.Errorf("dataset %s: bad file_pattern: %w", name, err)
    }

    for _, g := range []string{"key", "year"} {
        if re.SubexpIndex(g) < 0 {
            return nil, fmt.Errorf(
                "dataset %s: file_pattern needs a (?P<%s>...) group", name, g)
        }
    }

    bounds := box
    if b, ok := cfg.Boxes[box]; ok {
        bounds = b
    }

    ops := make([]string, 0, len(d.Ops))
    for _, op := range d.Ops {
        ops = append(ops, strings.ReplaceAll(op, "{box}", bounds))
    }

    short := name
    if i := strings.IndexByte(name, '_'); i >= 0 && i+1 < len(name) {
        short = name[i+1:]
    }

    r := &Resolved{
        Name: name,
        Description: d.Description,
        Root: root,
        Stream: d.Stream,
        Case: d.Case,
        Variable: d.Variable,
        Templates: templates,
        Pattern: re,
        Ops: ops,
        PieceDir: firstNonEmpty(d.PieceDir, short),
        PiecePrefix: firstNonEmpty(d.PiecePrefix, short),
        MergedName: firstNonEmpty(d.MergedName, name),
    }

    return r, nil
}

func firstNonEmpty(vals ...string) string {
    for _, v := range vals {
        if v != "" {
            return v
        }
    }

    return ""
}

// ------------------ //
// `Resolved` methods //
// ------------------ //

// expand fills a path template
func (r *Resolved) expand(t string, year int) string {
	rep := strings.NewReplacer(
		"{root}", r.Root,
		"{stream}", r.Stream,
		"{case}", r.Case,
		"{variable}", r.Variable,
		"{year}", strconv.Itoa(year),
		"{decade0}", strconv.Itoa((year/10)*10),
		"{decade9}", strconv.Itoa((year/10)*10+9),
	)
	return rep.Replace(t)
}

// SearchDirs returns every directory that might hold files for a year range,
// deduplicated and in a stable order
func (r *Resolved) SearchDirs(years []int) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, t := range r.Templates {
		monthly := strings.Contains(t, "{month}")
		for _, y := range years {
			base := r.expand(t, y)
			if !monthly {
				add(base)
				continue
			}
			for m := 1; m <= 12; m++ {
				add(strings.ReplaceAll(base, "{month}",
					fmt.Sprintf("%02d", m)))
			}
		}
	}
	return out
}

