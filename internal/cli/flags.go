package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/config"
)

// Go's flag package stops at the first non-flag argument, which would reject
// `quorum babysit 1811 --effort high "focus on the calendar"` even though both
// shell tools accepted exactly that shape. This parser keeps that freedom:
// options and positionals may be interleaved, and both `--flag value` and
// `--flag=value` work.
type args struct {
	opts map[string]string
	pos  []string
}

// flagSet is one command's options: the name without dashes, and whether the
// option takes a value. It is the single list the parser, the unknown-option
// check and the test that compares help text and reference against it share.
type flagSet map[string]bool

const (
	noValue    = false
	takesValue = true
)

// parseArgs splits argv. Knowing which options take no value is what makes
// `--dry-run 123` parse as a flag plus a positional rather than as a flag with
// the value "123".
func parseArgs(argv []string, flags flagSet) (*args, error) {
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
			if takes, known := flags[key]; known && !takes {
				// A switch is on by being there, so `--no-direnv=0` has to
				// mean off rather than count as present.
				on, err := switchValue(value)
				if err != nil {
					return nil, fmt.Errorf("--%s is a switch: %v", key, err)
				}
				if on {
					a.opts[key] = "1"
				} else {
					delete(a.opts, key)
				}
				continue
			}
			a.opts[key] = value
			continue
		}
		takes, known := flags[name]
		if known && !takes {
			a.opts[name] = "1"
			continue
		}
		if i+1 >= len(argv) {
			if !known {
				return nil, fmt.Errorf("unknown option: %s", arg)
			}
			return nil, fmt.Errorf("%s requires a value", arg)
		}
		i++
		a.opts[name] = argv[i]
	}
	return a, nil
}

// switchValue reads the value written after a switch, as in `--post=0`.
func switchValue(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("expected 1 or 0, got %q", v)
}

// plainArgs parses the arguments of a command that has no help page of its
// own. Without it such a command ignored whatever followed its name, so
// `quorum uninstall --help` removed the agent and `quorum gc --dryrun` deleted
// cache. done reports that the command must not run: the usage line was asked
// for and printed, or an argument was wrong.
func (a *app) plainArgs(usage string, argv []string, flags flagSet, positionals int) (parsed *args, code int, done bool) {
	accepted := flagSet{"h": noValue, "help": noValue}
	for name, takes := range flags {
		accepted[name] = takes
	}
	parsed, err := parseArgs(argv, accepted)
	if err == nil {
		if bad := parsed.unknown(accepted); len(bad) > 0 {
			err = fmt.Errorf("unknown option: --%s", bad[0])
		}
	}
	if err == nil && parsed.boolean("h", "help") {
		fmt.Fprintln(a.out.Out, "usage: "+usage)
		return nil, exitOK, true
	}
	if err == nil && len(parsed.pos) > positionals {
		err = fmt.Errorf("unexpected argument: %s", parsed.pos[positionals])
	}
	if err != nil {
		return nil, a.die("%v\nusage: %s", err, usage), true
	}
	return parsed, exitOK, false
}

func (a *args) has(names ...string) bool {
	for _, n := range names {
		if _, ok := a.opts[n]; ok {
			return true
		}
	}
	return false
}

func (a *args) str(fallback string, names ...string) string {
	for _, n := range names {
		if v, ok := a.opts[n]; ok {
			return v
		}
	}
	return fallback
}

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

// unknown returns the options that are not in the command's set, so a typo
// fails loudly instead of being silently ignored.
func (a *args) unknown(flags flagSet) []string {
	var out []string
	for k := range a.opts {
		if _, ok := flags[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
