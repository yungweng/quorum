package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/config"
)

// args holds a parsed command line.
//
// Go's flag package stops at the first non-flag argument, which would reject
// `quorum babysit 1811 --effort high "focus on the calendar"` even though both
// shell tools accepted exactly that shape. This parser keeps that freedom:
// options and positionals may be interleaved, and both `--flag value` and
// `--flag=value` work.
type args struct {
	opts map[string]string
	pos  []string
}

// parseArgs splits argv. boolFlags names the options that take no value, which
// is what makes `--dry-run 123` parse as a flag plus a positional rather than
// as a flag with the value "123".
func parseArgs(argv []string, boolFlags map[string]bool) (*args, error) {
	a := &args{opts: map[string]string{}}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			a.pos = append(a.pos, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if name == "" {
			return nil, fmt.Errorf("unknown option: %s", arg)
		}
		if key, value, ok := strings.Cut(name, "="); ok {
			a.opts[key] = value
			continue
		}
		if boolFlags[name] {
			a.opts[name] = "1"
			continue
		}
		if i+1 >= len(argv) {
			return nil, fmt.Errorf("%s requires a value", arg)
		}
		i++
		a.opts[name] = argv[i]
	}
	return a, nil
}

// has reports whether an option was given at all.
func (a *args) has(names ...string) bool {
	for _, n := range names {
		if _, ok := a.opts[n]; ok {
			return true
		}
	}
	return false
}

// str returns the first of names that was given, or fallback.
func (a *args) str(fallback string, names ...string) string {
	for _, n := range names {
		if v, ok := a.opts[n]; ok {
			return v
		}
	}
	return fallback
}

// boolean reports whether a flag was set.
func (a *args) boolean(names ...string) bool { return a.has(names...) }

func (a *args) intVal(fallback int, names ...string) (int, error) {
	for _, n := range names {
		v, ok := a.opts[n]
		if !ok {
			continue
		}
		i, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, fmt.Errorf("--%s must be a number", n)
		}
		return i, nil
	}
	return fallback, nil
}

func (a *args) duration(fallback time.Duration, names ...string) (time.Duration, error) {
	for _, n := range names {
		v, ok := a.opts[n]
		if !ok {
			continue
		}
		d, err := config.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("--%s: %v", n, err)
		}
		return d, nil
	}
	return fallback, nil
}

// unknown returns the options that are not in the allowed set, so a typo fails
// loudly instead of being silently ignored.
func (a *args) unknown(allowed map[string]bool) []string {
	var out []string
	for k := range a.opts {
		if !allowed[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
