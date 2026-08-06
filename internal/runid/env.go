package runid

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

var (
	envOnce sync.Once
	envID   RunID
	envSet  bool
	envErr  error
)

// FromEnv reads the ambient run identity exactly once per process and returns
// the same answer forever after. The single read is the immutability
// guarantee: whatever the environment does later — including another
// package calling os.Setenv — cannot change the identity this process
// already attached to its artifacts.
//
// The three outcomes are distinct: absent (set=false, err=nil), present and
// valid (set=true, err=nil), and present but malformed (set=true, err!=nil).
// A malformed ambient id is never silently ignored.
func FromEnv() (RunID, bool, error) {
	envOnce.Do(func() {
		envID, envSet, envErr = lookupRunID(os.LookupEnv)
	})
	return envID, envSet, envErr
}

func lookupRunID(lookup func(string) (string, bool)) (RunID, bool, error) {
	raw, ok := lookup(EnvVar)
	if !ok {
		return "", false, nil
	}
	id, err := Parse(raw)
	if err != nil {
		return "", true, fmt.Errorf("invalid %s: %w", EnvVar, err)
	}
	return id, true, nil
}

// InjectEnv returns a copy of base carrying exactly one canonical run-identity
// entry. Pre-existing entries for the variable are removed rather than
// shadowed, so a child can never observe two values and pick the wrong one.
//
// base is typically os.Environ(); it is not modified. The result is meant for
// exec.Cmd.Env — this is the only supported way to propagate the identity
// outward, and the reason os.Setenv is never called anywhere in this package.
//
// Removal matches the variable name case-insensitively: environment names are
// case-insensitive on Windows, so a differently-cased leftover would collide
// there. Only the canonical uppercase name is ever written.
func InjectEnv(base []string, id RunID) ([]string, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("runid: inject: no run identity to propagate")
	}
	if _, err := Parse(id.String()); err != nil {
		return nil, fmt.Errorf("runid: inject: %w", err)
	}
	out := make([]string, 0, len(base)+1)
	for _, entry := range base {
		if strings.EqualFold(envKey(entry), EnvVar) {
			continue
		}
		out = append(out, entry)
	}
	return append(out, EnvVar+"="+id.String()), nil
}

// envKey returns the variable name of an environment entry. An entry without
// '=' is treated as a bare name, which is how a stray "NEUROFS_RUN_ID" entry
// still gets removed instead of reaching the child.
func envKey(entry string) string {
	if i := strings.IndexByte(entry, '='); i >= 0 {
		return entry[:i]
	}
	return entry
}

// Resolve reconciles an explicit run identity (a flag, an argument) with the
// ambient one from the environment.
//
// A disagreement is an error: the run's artifacts would be labelled with one
// id while the caller believes another, which is exactly the mislabelling
// this whole layer exists to prevent. Agreement is accepted, and either one
// alone is accepted.
func Resolve(explicit string) (RunID, bool, error) {
	ambient, ambientSet, ambientErr := FromEnv()
	return resolve(explicit, ambient, ambientSet, ambientErr)
}

func resolve(explicit string, ambient RunID, ambientSet bool, ambientErr error) (RunID, bool, error) {
	if explicit == "" {
		if ambientErr != nil {
			return "", true, ambientErr
		}
		if !ambientSet {
			return "", false, nil
		}
		return ambient, true, nil
	}

	id, err := Parse(explicit)
	if err != nil {
		return "", true, fmt.Errorf("explicit run id: %w", err)
	}
	if ambientErr != nil {
		return "", true, fmt.Errorf("explicit run id %q given alongside an %w", id, ambientErr)
	}
	if ambientSet && ambient != id {
		return "", true, fmt.Errorf(
			"runid: conflicting run identity: explicit %q vs %s %q — refusing to overwrite silently",
			id, EnvVar, ambient)
	}
	return id, true, nil
}
