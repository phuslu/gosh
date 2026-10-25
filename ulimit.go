//go:build !windows

package gosh

import (
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"

	"golang.org/x/sys/unix"
)

// ulimitResource is one limit ulimit reports, in the order of "ulimit -a".
type ulimitResource struct {
	flag        byte
	description string
	units       string
	resource    int // -1 for the read-only pipe size
	factor      uint64
}

const ulimitUsage = "ulimit: usage: ulimit [-SHabcdefiklmnpqrstuvxPRT] [limit]"

// builtinUlimit implements Bash's ulimit: it prints the soft limit, or the
// hard one with -H, of each resource named by an option, -f by default and
// all with -a, and sets them to a number in the resource's units,
// "unlimited", "soft" or "hard". Setting changes both limits unless -S or
// -H picks one. Limits belong to the gosh process and the commands it
// starts.
func builtinUlimit(args []string, stdout io.Writer) error {
	soft, hard, all := false, false, false
	var picked []ulimitResource
	for len(args) > 0 && len(args[0]) > 1 && args[0][0] == '-' && !isNumber(args[0]) {
		if args[0] == "--" {
			args = args[1:]
			break
		}
		for _, opt := range []byte(args[0][1:]) {
			switch opt {
			case 'S':
				soft = true
			case 'H':
				hard = true
			case 'a':
				all = true
			default:
				res, ok := findUlimitResource(opt)
				if !ok {
					return builtinUsagef("ulimit: -%c: invalid option\n%s", opt, ulimitUsage)
				}
				picked = append(picked, res)
			}
		}
		args = args[1:]
	}
	if all {
		for _, res := range ulimitResources {
			if err := printUlimit(stdout, res, hard, true); err != nil {
				return err
			}
		}
		return nil
	}
	if len(picked) == 0 {
		picked = []ulimitResource{mustUlimitResource('f')}
	}
	if len(args) == 0 {
		for _, res := range picked {
			if err := printUlimit(stdout, res, hard, len(picked) > 1); err != nil {
				return err
			}
		}
		return nil
	}
	for _, res := range picked {
		if err := setUlimit(res, args[0], soft, hard); err != nil {
			return err
		}
	}
	return nil
}

func findUlimitResource(flag byte) (ulimitResource, bool) {
	for _, res := range ulimitResources {
		if res.flag == flag {
			return res, true
		}
	}
	return ulimitResource{}, false
}

func mustUlimitResource(flag byte) ulimitResource {
	res, _ := findUlimitResource(flag)
	return res
}

func getUlimit(res ulimitResource) (unix.Rlimit, error) {
	if res.resource < 0 {
		// The pipe size is fixed: PIPE_BUF bytes, in 512-byte blocks.
		return unix.Rlimit{Cur: 8, Max: 8}, nil
	}
	var lim unix.Rlimit
	err := unix.Getrlimit(res.resource, &lim)
	return lim, err
}

func printUlimit(w io.Writer, res ulimitResource, hard, labeled bool) error {
	lim, err := getUlimit(res)
	if err != nil {
		return fmt.Errorf("ulimit: %s: cannot get limit: %v", res.description, err)
	}
	value := uint64(lim.Cur)
	if hard {
		value = uint64(lim.Max)
	}
	text := "unlimited"
	if value != unix.RLIM_INFINITY {
		text = strconv.FormatUint(value/res.factor, 10)
	}
	if labeled {
		unit := fmt.Sprintf("(-%c) ", res.flag)
		if res.units != "" {
			unit = fmt.Sprintf("(%s, -%c) ", res.units, res.flag)
		}
		fmt.Fprintf(w, "%-20s %20s", res.description, unit)
	}
	fmt.Fprintln(w, text)
	return nil
}

func setRlimitField(field any, n uint64) {
	v := reflect.ValueOf(field).Elem()
	if v.CanUint() {
		v.SetUint(n)
	} else {
		v.SetInt(int64(min(n, math.MaxInt64)))
	}
}

func setUlimit(res ulimitResource, value string, soft, hard bool) error {
	lim, err := getUlimit(res)
	if err != nil {
		return fmt.Errorf("ulimit: %s: cannot get limit: %v", res.description, err)
	}
	var n uint64
	switch value {
	case "unlimited":
		n = unix.RLIM_INFINITY
	case "soft":
		n = uint64(lim.Cur)
	case "hard":
		n = uint64(lim.Max)
	default:
		v, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return fmt.Errorf("ulimit: %s: invalid number", value)
		}
		if v > math.MaxUint64/res.factor {
			return fmt.Errorf("ulimit: %s: limit out of range", value)
		}
		n = v * res.factor
	}
	if res.resource < 0 {
		return fmt.Errorf("ulimit: %s: cannot modify limit: %s", res.description, strerror(unix.EINVAL))
	}
	if !soft && !hard {
		soft, hard = true, true
	}
	// The fields are signed on some systems and unsigned on others.
	if soft {
		setRlimitField(&lim.Cur, n)
	}
	if hard {
		setRlimitField(&lim.Max, n)
	}
	if err := unix.Setrlimit(res.resource, &lim); err != nil {
		return fmt.Errorf("ulimit: %s: cannot modify limit: %s", res.description, strerror(err))
	}
	return nil
}
