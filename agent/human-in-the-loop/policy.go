// Package humanintheloop answers one question for the agent loop: does this
// tool call have to wait for a person?
//
// The answer comes from HITL_ENABLED in .env and from hitl_config.yml, both
// read relative to the working directory. Callers see neither - the shape of
// the file stays here, so changing it does not reach into the loop.
package humanintheloop

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// ConfigFile names the policy file, read from the working directory like .env.
const ConfigFile = "hitl_config.yml"

// config is the YAML as written. Unexported: nothing outside this package has
// to know how the file is laid out.
type config struct {
	Tools struct {
		RequireApproval []string `yaml:"require_approval"`
	} `yaml:"tools"`
}

// policy is one loaded reading of the flag and the file.
type policy struct {
	enabled bool
	require map[string]bool

	// gateEverything is set when the file is there but unusable. A broken
	// config fails closed, since the safe reading of "I could not tell" is to
	// ask a human.
	gateEverything bool
}

// The loaded policy, kept because a tool call is a hot path and the file is
// not. A pointer swapped under the lock rather than a mutated struct, so a
// reload cannot be seen half-applied.
var (
	mu     sync.RWMutex
	loaded *policy
)

// RequiresApproval reports whether the given tool needs human approval before
// it runs. Always false while HITL_ENABLED is off.
func RequiresApproval(tool string) bool {
	p := current()
	if !p.enabled {
		return false
	}
	if p.gateEverything {
		return true
	}

	return p.require[tool]
}

// Enabled reports whether HITL interception is on at all.
func Enabled() bool { return current().enabled }

// Reload re-reads .env and the config file, replacing what is held. The error
// describes a config that could not be read; the policy is still replaced,
// with one that gates every tool.
func Reload() error {
	p, err := load()

	mu.Lock()
	loaded = p
	mu.Unlock()

	return err
}

// current returns the held policy, loading it on first use.
func current() *policy {
	mu.RLock()
	p := loaded
	mu.RUnlock()
	if p != nil {
		return p
	}

	mu.Lock()
	defer mu.Unlock()
	if loaded == nil {
		p, err := load()
		if err != nil {
			// Nobody asked for this load, so the error has nowhere to go but
			// the terminal. Silence would leave every call pausing unexplained.
			fmt.Fprintf(os.Stderr, "human-in-the-loop: %v (every tool call will need approval)\n", err)
		}
		loaded = p
	}

	return loaded
}

// load reads the flag and the file. It always returns a usable policy, so a
// caller that ignores the error still gets the fail-closed one.
func load() (*policy, error) {
	p := &policy{require: map[string]bool{}}

	// A missing .env is not an error here: no flag means HITL is off.
	env, _ := godotenv.Read(".env")
	p.enabled, _ = strconv.ParseBool(strings.TrimSpace(env["HITL_ENABLED"]))

	data, err := os.ReadFile(ConfigFile)
	if errors.Is(err, fs.ErrNotExist) {
		// No file means nothing was declared, so nothing is gated.
		return p, nil
	}
	if err != nil {
		p.gateEverything = true
		return p, fmt.Errorf("read %s: %w", ConfigFile, err)
	}

	var cfg config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		p.gateEverything = true
		return p, fmt.Errorf("parse %s: %w", ConfigFile, err)
	}

	for _, tool := range cfg.Tools.RequireApproval {
		if tool = strings.TrimSpace(tool); tool != "" {
			p.require[tool] = true
		}
	}

	return p, nil
}
