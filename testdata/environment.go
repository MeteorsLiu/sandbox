//go:build linux && (arm64 || amd64) && cgo

package main

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/xgo-dev/sandbox"
)

var environmentAtInit = os.Getenv("SANDBOX_ENV_TEST")

func checkEnvironment() error {
	const key = "SANDBOX_ENV_TEST"
	original, existed := os.LookupEnv(key)
	defer func() {
		if existed {
			os.Setenv(key, original)
		} else {
			os.Unsetenv(key)
		}
	}()
	for _, test := range []struct {
		name, host, want string
		env              []string
	}{
		{name: "inherit", host: "host-first", want: "host-first"},
		{name: "inherit changed host", host: "host-second", want: "host-second"},
		{name: "empty", host: "host", env: []string{}},
		{name: "explicit", host: "host", want: "guest", env: []string{"PATH=/usr/bin:/bin", key + "=guest", "EMPTY=", "WITH_EQUALS=a=b"}},
	} {
		if err := os.Setenv(key, test.host); err != nil {
			return err
		}
		want := slices.Clone(test.env)
		if test.env == nil {
			want = os.Environ()
		}
		var actual []string
		var initial, child string
		s := sandbox.Sandbox{Env: test.env}
		if err := s.Run(func() {
			actual = os.Environ()
			initial = environmentAtInit
			if os.Getenv("PATH") != "" {
				out, err := exec.Command("sh", "-c", `printf %s "$SANDBOX_ENV_TEST"`).Output()
				if err != nil {
					panic(err)
				}
				child = string(out)
			}
			if err := os.Setenv("SANDBOX_ENV_TEST", "guest-change"); err != nil {
				panic(err)
			}
		}); err != nil {
			return fmt.Errorf("environment %s: %w", test.name, err)
		}
		slices.Sort(want)
		slices.Sort(actual)
		if !slices.Equal(actual, want) || initial != test.want || child != test.want || os.Getenv(key) != test.host {
			return fmt.Errorf("environment %s: entries match=%t init=%q child=%q host=%q", test.name, slices.Equal(actual, want), initial, child, os.Getenv(key))
		}
	}
	invalid := sandbox.Sandbox{Env: []string{"INVALID=before\x00after"}}
	if err := invalid.Run(staticCall); err == nil || !strings.Contains(err.Error(), "guest environment contains NUL") {
		return fmt.Errorf("environment NUL was not rejected: %v", err)
	}
	fmt.Println("PASS inherited, refreshed, empty and explicit guest environments, package init, child processes, host isolation and NUL rejection")
	return nil
}

func checkErrorMessage() error {
	s := sandbox.Sandbox{Mounts: []sandbox.Mount{
		{Type: "bind", Source: "/", Target: "/"},
		{Type: strings.Repeat("x", 8192), Target: "/invalid"},
	}}
	err := s.Run(staticCall)
	if err == nil {
		return fmt.Errorf("unsupported filesystem accepted")
	}
	message := strings.TrimPrefix(err.Error(), "sandbox Sentry: ")
	if len(message) != 4095 || !strings.HasPrefix(message, "unsupported filesystem type") {
		return fmt.Errorf("error message was not bounded: length=%d", len(message))
	}
	if err := sandbox.Run(staticCall); err != nil {
		return fmt.Errorf("run after truncated error: %w", err)
	}
	fmt.Println("PASS oversized C bridge error is truncated and the following Run succeeds")
	return nil
}
