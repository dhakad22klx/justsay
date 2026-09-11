// Package humanintheloop answers one question for the agent loop: does this
// tool call have to wait for a person?
//
// The answer comes from HITL_ENABLED in .env and from hitl_config.yml, both
// read relative to the working directory. The shape of that file stays here,
// so changing it does not reach into the loop.
package humanintheloop

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// ConfigFile is the policy file, named from the repository root like .env is.
const ConfigFile = "agent/human-in-the-loop/hitl_config.yml"

// config is the YAML as written, kept unexported so the file's shape stops
// at this package.
type config struct {
	Tools struct {
		RequireApproval []string `yaml:"require_approval"`
	} `yaml:"tools"`
}

// policy is one reading of the flag and the file. gateEverything is set when
// the file is there but unusable: "I cannot tell" fails closed.
type policy struct {
	enabled        bool
	gateEverything bool
	require        map[string]bool
}

// The policy, held because it is asked about far more often than it changes.
var (
	mu     sync.Mutex
	loaded *policy
)

// RequiresApproval reports whether the given tool needs human approval before
// it runs. Always false while HITL_ENABLED is off.
func RequiresApproval(tool string) bool {
	p := current()

	return p.enabled && (p.gateEverything || p.require[tool])
}

// Reload re-reads .env and the config file. A config that could not be read is
// still installed - as the one that gates everything - and reported here.
func Reload() error {
	p, err := load()

	mu.Lock()
	loaded = p
	mu.Unlock()

	return err
}

// current returns the held policy, loading it on first use.
func current() *policy {
	mu.Lock()
	defer mu.Unlock()

	if loaded == nil {
		var err error
		// Nobody asked for this load, so a bad config has nowhere to report to
		// but the terminal. Silence would leave every call pausing unexplained.
		if loaded, err = load(); err != nil {
			fmt.Fprintf(os.Stderr, "human-in-the-loop: %v; every tool call will need approval\n", err)
		}
	}

	return loaded
}

// load always returns a usable policy, so a caller that ignores the error
// still gets the fail-closed one.
func load() (*policy, error) {
	// A missing .env is not an error here: no flag means HITL is off.
	env, _ := godotenv.Read(".env")
	enabled, _ := strconv.ParseBool(strings.TrimSpace(env["HITL_ENABLED"]))
	p := &policy{enabled: enabled, require: map[string]bool{}}

	data, err := os.ReadFile(ConfigFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return p, nil // nothing declared, so nothing is gated
		}
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
