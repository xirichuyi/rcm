package mirror

import (
	"bufio"
	"errors"
	"io/fs"
	"path"
	"regexp"
	"strings"
)

var defaultIgnores = []string{"node_modules/", ".venv/", "venv/", "__pycache__/", "dist/", "build/", "target/", "coverage/", ".next/", ".cache/", "*.log", ".DS_Store"}

type rule struct {
	base     string
	re       *regexp.Regexp
	neg, dir bool
}
type IgnoreRule struct {
	Base  string `json:"base"`
	Lines string `json:"lines"`
}
type Ignore struct {
	rules      []rule
	includeGit bool
	sources    []IgnoreRule
}

func (i *Ignore) Add(base, lines string) {
	i.sources = append(i.sources, IgnoreRule{base, lines})
	s := bufio.NewScanner(strings.NewReader(lines))
	for s.Scan() {
		line := strings.TrimSuffix(s.Text(), "\r")
		line = strings.TrimRight(line, " ")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r := rule{base: base}
		if strings.HasPrefix(line, "!") {
			r.neg = true
			line = line[1:]
		}
		line = strings.TrimPrefix(line, "\\")
		r.dir = strings.HasSuffix(line, "/")
		line = strings.TrimSuffix(line, "/")
		anchor := strings.HasPrefix(line, "/") || strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		var b strings.Builder
		if anchor {
			b.WriteString("^")
		} else {
			b.WriteString("(^|/)")
		}
		for n := 0; n < len(line); n++ {
			switch line[n] {
			case '*':
				if n+1 < len(line) && line[n+1] == '*' {
					n++
					if n+1 < len(line) && line[n+1] == '/' {
						n++
						b.WriteString("(?:.*/)?")
					} else {
						b.WriteString(".*")
					}
				} else {
					b.WriteString("[^/]*")
				}
			case '?':
				b.WriteString("[^/]")
			case '[':
				end := strings.IndexByte(line[n+1:], ']')
				if end >= 0 {
					group := line[n : n+end+2]
					if strings.HasPrefix(group, "[!") {
						group = "[^" + group[2:]
					}
					b.WriteString(group)
					n += end + 1
				} else {
					b.WriteString("\\[")
				}
			default:
				b.WriteString(regexp.QuoteMeta(string(line[n])))
			}
		}
		b.WriteString("$")
		re, err := regexp.Compile(b.String())
		if err == nil {
			r.re = re
			i.rules = append(i.rules, r)
		}
	}
}
func (i *Ignore) Match(p string, dir bool) bool {
	for _, s := range strings.Split(p, "/") {
		if s == Internal {
			return true
		}
	}
	if GitPath(p) && !i.includeGit {
		return true
	}
	ignored := false
	for _, r := range i.rules {
		rel := p
		if r.base != "" {
			if !strings.HasPrefix(p, r.base+"/") {
				continue
			}
			rel = strings.TrimPrefix(p, r.base+"/")
		}
		if r.dir && !dir {
			continue
		}
		if r.re.MatchString(rel) {
			ignored = !r.neg
		}
	}
	return ignored
}
func LoadIgnore(tree *Tree, c Config) (*Ignore, error) {
	i := &Ignore{includeGit: c.IncludeGit}
	i.Add("", strings.Join(defaultIgnores, "\n"))
	// Read rules through the root capability. Symlinks are never traversed.
	var walk func(string) error
	walk = func(dir string) error {
		if dir != "." {
			if err := tree.Parents(dir + "/placeholder"); err != nil {
				return err
			}
		}
		f, err := tree.Root.Open(dir)
		if err != nil {
			return err
		}
		ents, err := f.ReadDir(-1)
		f.Close()
		if err != nil {
			return err
		}
		name := path.Join(dir, ".gitignore")
		if info, e := tree.Root.Lstat(name); e == nil && info.Mode().IsRegular() {
			b, e := tree.Root.ReadFile(name)
			if e != nil {
				return e
			}
			base := dir
			if base == "." {
				base = ""
			}
			i.Add(base, string(b))
		}
		for _, d := range ents {
			p := path.Join(dir, d.Name())
			if d.IsDir() && !i.Match(p, true) {
				if err := walk(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
		}
		return nil
	}
	if err := walk("."); err != nil {
		return nil, err
	}
	if info, err := tree.Root.Lstat(".rcmignore"); err == nil && info.Mode().IsRegular() {
		b, err := tree.Root.ReadFile(".rcmignore")
		if err != nil {
			return nil, err
		}
		i.Add("", string(b))
	}
	i.Add("", strings.Join(c.Ignore, "\n"))
	return i, nil
}
