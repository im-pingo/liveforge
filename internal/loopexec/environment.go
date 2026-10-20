package loopexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

func environment(profile string) []string {
	values := make(map[string]string)
	for _, item := range os.Environ() {
		k, v, ok := strings.Cut(item, "=")
		if ok {
			values[k] = v
		}
	}
	values["CGO_ENABLED"] = "0"
	if profile == "tagged" {
		values["CGO_ENABLED"] = "1"
	}
	values["GOFLAGS"] = ""
	// Preserve Go's configured toolchain selection, including go env -w settings.
	values["GOWORK"] = "off"
	values["GOOS"] = runtime.GOOS
	values["GOARCH"] = runtime.GOARCH
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, k := range keys {
		result = append(result, k+"="+values[k])
	}
	return result
}
func envValue(env []string, key string) string {
	for _, item := range env {
		if k, v, ok := strings.Cut(item, "="); ok && k == key {
			return v
		}
	}
	return ""
}
func setEnv(env []string, key, value string) []string {
	result := append([]string(nil), env...)
	for i, item := range result {
		if strings.HasPrefix(item, key+"=") {
			result[i] = key + "=" + value
			return result
		}
	}
	return append(result, key+"="+value)
}
func recordedEnvironment(env []string) map[string]string {
	result := make(map[string]string)
	for _, key := range []string{"CGO_ENABLED", "GOFLAGS", "GOTOOLCHAIN", "GOWORK", "GOOS", "GOARCH", "CHROME_BIN", "LF_BINARY", "LIVEFORGE_REQUIRE_BROWSER", "LIVEFORGE_PROTOCOL_MATRIX_SOAK"} {
		for _, item := range env {
			if k, v, ok := strings.Cut(item, "="); ok && k == key {
				result[k] = v
			}
		}
	}
	return result
}
func probe(ctx context.Context, root string, env []string, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	out := &limitedLog{writer: &stdout, remaining: 16 << 10}
	errout := &limitedLog{writer: &stderr, remaining: 16 << 10}
	result := runProcess(ctx, root, argv, env, out, errout)
	if result.Err != nil || result.ExitCode != 0 {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = fmt.Sprint(result.Err)
		}
		return "", fmt.Errorf("%s prerequisite: %s", argv[0], detail)
	}
	return strings.TrimSpace(stdout.String()), nil
}

var goVersionPattern = regexp.MustCompile(`go([0-9]+)\.([0-9]+)`)

func prerequisites(ctx context.Context, root string, check Check, profile string, env []string, tools map[string]string, cache map[string]error) ([]string, error) {
	if check.Kind == "go-test" {
		if previous, ok := cache["go"]; ok {
			if previous != nil {
				return env, previous
			}
		} else {
			version, err := probe(ctx, root, env, "go", "version")
			if err == nil {
				match := goVersionPattern.FindStringSubmatch(version)
				if len(match) != 3 {
					err = errors.New("cannot determine Go version")
				} else {
					major, _ := strconv.Atoi(match[1])
					minor, _ := strconv.Atoi(match[2])
					if major < 1 || (major == 1 && minor < 26) {
						err = errors.New("requires Go 1.26 or newer")
					}
				}
			}
			cache["go"] = err
			tools["go"] = version
			if err != nil {
				return env, err
			}
		}
		if profile == "tagged" {
			if previous, ok := cache["cgo"]; ok {
				if previous != nil {
					return env, previous
				}
			} else {
				compiler, err := probe(ctx, root, env, "go", "env", "CC")
				if err == nil {
					argv := strings.Fields(compiler)
					if len(argv) == 0 {
						err = errors.New("no C compiler configured")
					} else {
						version, probeErr := probe(ctx, root, env, append(argv, "--version")...)
						tools["cc"] = version
						err = probeErr
					}
				}
				cache["cgo"] = err
				if err != nil {
					return env, err
				}
			}
		}
	}
	if check.RequireBrowser {
		env = setEnv(env, "LIVEFORGE_REQUIRE_BROWSER", "1")
		browser := envValue(env, "CHROME_BIN")
		if browser == "" {
			for _, candidate := range []string{"google-chrome", "chromium", "chromium-browser", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"} {
				if path, err := exec.LookPath(candidate); err == nil {
					browser = path
					break
				}
			}
		}
		if browser == "" {
			return env, errors.New("required browser unavailable; set CHROME_BIN")
		}
		if strings.ContainsRune(browser, filepath.Separator) && !filepath.IsAbs(browser) {
			browser = filepath.Join(root, browser)
		}
		path, err := exec.LookPath(browser)
		if err != nil {
			return env, fmt.Errorf("required CHROME_BIN: %w", err)
		}
		env = setEnv(env, "CHROME_BIN", path)
		key := "browser:" + path
		if previous, ok := cache[key]; ok {
			if previous != nil {
				return env, previous
			}
		} else {
			version, err := probe(ctx, root, env, path, "--version")
			cache[key] = err
			tools["browser"] = version
			if err != nil {
				return env, err
			}
		}
	}
	return env, nil
}

// Used only for Go build failures before any test runs, never for assertions.
func environmentDiagnostic(text string) bool {
	lower := strings.ToLower(text)
	for _, needle := range []string{
		"you have not agreed to the xcode license",
		"cannot find -lavcodec", "cannot find -lswresample",
		"library not found for -lavcodec", "library not found for -lswresample",
		"package libavcodec was not found", "'libavcodec/avcodec.h' file not found",
		"libavcodec/avcodec.h: no such file",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}
