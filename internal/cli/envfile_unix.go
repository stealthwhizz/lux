//go:build !windows

package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// envFileKeys are the variables an env file may set besides LUX_*: the
// provider key names luxd also accepts.
var envFileKeys = map[string]bool{"OPENAI_API_KEY": true, "ANTHROPIC_API_KEY": true, "LYZR_API_KEY": true}

// loadEnvFile reads KEY=VALUE lines for luxd (blank lines, # comments and an
// `export ` prefix are allowed; values are taken literally, with one pair of
// surrounding quotes removed). The file can set the command luxd runs
// (LUX_RUNNER_CMD), so it must belong to this user and not be writable by
// anyone else. Errors name the line, never the value.
func loadEnvFile(path string) ([]string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("%s belongs to another user; refusing to load it", path)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s can be changed by other users; run: chmod 600 %s", path, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var env []string
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("%s line %d: expected KEY=VALUE", path, n)
		}
		if !strings.HasPrefix(key, "LUX_") && !envFileKeys[key] {
			return nil, fmt.Errorf("%s line %d: %s is not a Lux setting (see .env.example)", path, n, key)
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if val != "" { // an empty value means "not set", as in .env.example
			env = append(env, key+"="+val)
		}
	}
	return env, sc.Err()
}

// envValue returns the last value of key in env.
func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, _ := strings.Cut(kv, "="); k == key {
			v = val
		}
	}
	return v
}
