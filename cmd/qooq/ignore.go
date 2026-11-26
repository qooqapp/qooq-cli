package main

import (
	"bufio"
	"io"
	"regexp"
	"strings"
)

// ignoreRules is a small gitignore-syntax matcher, for .qooqignore.
//
// Hand-written because the CLI's dependency wall is deliberate and this needs
// a screenful, not a library. Supported: blank lines and # comments, `*`, `?`,
// `[...]`, `**` (leading `**/`, trailing `/**`, and `/**/` in the middle), a
// leading or inner `/` anchoring the pattern to the root, a trailing `/`
// matching directories only, `!` to re-include, and `\` to escape a leading
// `#` or `!`. As in git, a file inside an ignored directory stays ignored no
// matter what a later `!` says: the directory is never entered.
type ignoreRules struct{ rules []ignoreRule }

type ignoreRule struct {
	re      *regexp.Regexp
	negate  bool
	dirOnly bool
}

func parseIgnore(r io.Reader) *ignoreRules {
	out := &ignoreRules{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rule := ignoreRule{}
		if strings.HasPrefix(line, "!") {
			rule.negate = true
			line = line[1:]
		} else if strings.HasPrefix(line, `\#`) || strings.HasPrefix(line, `\!`) {
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			rule.dirOnly = true
			line = strings.TrimRight(line, "/")
		}
		if line == "" {
			continue
		}
		anchored := strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		prefix := "^"
		if !anchored {
			prefix = "(?:^|.*/)"
		}
		re, err := regexp.Compile(prefix + globToRegexp(line) + "$")
		if err != nil {
			continue // a pattern that cannot be read is ignored, as git does
		}
		rule.re = re
		out.rules = append(out.rules, rule)
	}
	return out
}

func globToRegexp(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch c {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				i++
				switch {
				case i+1 < len(g) && g[i+1] == '/': // "**/"
					i++
					b.WriteString(`(?:.*/)?`)
				case i+1 == len(g): // trailing "/**" or a lone "**"
					b.WriteString(`.*`)
				default:
					b.WriteString(`.*`)
				}
			} else {
				b.WriteString(`[^/]*`)
			}
		case '?':
			b.WriteString(`[^/]`)
		case '[':
			j := strings.IndexByte(g[i+1:], ']')
			if j < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := g[i+1 : i+1+j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += j + 1
		case '\\':
			if i+1 < len(g) {
				i++
				b.WriteString(regexp.QuoteMeta(string(g[i])))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// decide reports the verdict of the last rule matching rel (slash-separated),
// and whether any rule matched at all.
func (ir *ignoreRules) decide(rel string, isDir bool) (ignored, matched bool) {
	for _, r := range ir.rules {
		if r.dirOnly && !isDir {
			continue
		}
		if r.re.MatchString(rel) {
			ignored, matched = !r.negate, true
		}
	}
	return ignored, matched
}

// Ignored reports whether the file rel is excluded: either a rule excludes it,
// or one of the directories above it is excluded.
func (ir *ignoreRules) Ignored(rel string) bool {
	if ir == nil || len(ir.rules) == 0 {
		return false
	}
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		if ign, _ := ir.decide(strings.Join(parts[:i], "/"), true); ign {
			return true
		}
	}
	ign, _ := ir.decide(rel, false)
	return ign
}
