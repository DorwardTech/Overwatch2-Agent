package main

import (
	"errors"
	"flag"
	"path/filepath"
	"testing"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		cmd  string
		cfg  string
	}{
		{"bare invocation is the container entrypoint", nil, "run", ""},
		{"healthcheck keeps working", []string{"healthcheck"}, "healthcheck", ""},
		{"flags without a command mean run", []string{"--config", "x.env"}, "run", "x.env"},
		{"install with a config", []string{"install", "-config", "x.env"}, "install", "x.env"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, opts, err := parseArgs(c.args)
			if err != nil {
				t.Fatalf("parseArgs(%v): %v", c.args, err)
			}
			if cmd != c.cmd {
				t.Errorf("cmd = %q, want %q", cmd, c.cmd)
			}
			want := c.cfg
			if want != "" {
				// Made absolute so a service, which starts in System32, finds it.
				want, _ = filepath.Abs(want)
			}
			if opts.configPath != want {
				t.Errorf("configPath = %q, want %q", opts.configPath, want)
			}
		})
	}
}

func TestParseArgsErrors(t *testing.T) {
	if _, _, err := parseArgs([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("--help: err = %v, want flag.ErrHelp", err)
	}
	if _, _, err := parseArgs([]string{"run", "extra"}); err == nil {
		t.Error("a stray positional argument was accepted")
	}
	if _, _, err := parseArgs([]string{"--no-such-flag"}); err == nil {
		t.Error("an unknown flag was accepted")
	}
}

func TestHealthURL(t *testing.T) {
	cases := map[string]string{
		"":               "http://127.0.0.1:8088/healthz",
		":8088":          "http://127.0.0.1:8088/healthz",
		"0.0.0.0:9000":   "http://127.0.0.1:9000/healthz",
		"127.0.0.1:8088": "http://127.0.0.1:8088/healthz",
		"[::]:8088":      "http://127.0.0.1:8088/healthz",
	}
	for in, want := range cases {
		got, err := healthURL(in)
		if err != nil {
			t.Errorf("healthURL(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("healthURL(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := healthURL("no-port"); err == nil {
		t.Error("an address without a port was accepted")
	}
}
