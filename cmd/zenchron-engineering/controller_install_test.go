package main

import "testing"

func TestParseInstallFlags(t *testing.T) {
	for _, test := range []struct {
		name           string
		args           []string
		wantBinDir     string
		wantConfig     string
		wantErr        bool
		wantErrContain string
	}{
		{name: "nothing given", args: nil},
		{name: "bin dir", args: []string{"--bin-dir", "/opt/bin"}, wantBinDir: "/opt/bin"},
		{name: "config", args: []string{"--config", "/etc/zenchron.json"}, wantConfig: "/etc/zenchron.json"},
		{name: "both", args: []string{"--bin-dir", "/opt/bin", "--config", "/etc/zenchron.json"},
			wantBinDir: "/opt/bin", wantConfig: "/etc/zenchron.json"},
		{name: "bin dir missing its value", args: []string{"--bin-dir"}, wantErr: true, wantErrContain: "--bin-dir requires"},
		{name: "bin dir given an empty value", args: []string{"--bin-dir", "  "}, wantErr: true, wantErrContain: "--bin-dir requires"},
		{name: "bin dir given twice", args: []string{"--bin-dir", "/a", "--bin-dir", "/b"}, wantErr: true, wantErrContain: "more than once"},
		{name: "config missing its value", args: []string{"--config"}, wantErr: true, wantErrContain: "--config requires"},
		{name: "config given twice", args: []string{"--config", "/a", "--config", "/b"}, wantErr: true, wantErrContain: "more than once"},
		{name: "unknown flag", args: []string{"--force"}, wantErr: true, wantErrContain: "does not accept"},
	} {
		t.Run(test.name, func(t *testing.T) {
			binDir, config, err := parseInstallFlags(test.args)
			if test.wantErr {
				if err == nil {
					t.Fatalf("want an error, got binDir=%q config=%q", binDir, config)
				}
				if test.wantErrContain != "" && !containsString(err.Error(), test.wantErrContain) {
					t.Fatalf("error %q does not mention %q", err.Error(), test.wantErrContain)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if binDir != test.wantBinDir {
				t.Fatalf("binDir = %q, want %q", binDir, test.wantBinDir)
			}
			if config != test.wantConfig {
				t.Fatalf("config = %q, want %q", config, test.wantConfig)
			}
		})
	}
}

func containsString(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
