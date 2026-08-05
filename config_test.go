package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveConfig(t *testing.T) {
	dir := t.TempDir()

	writeConfig := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}

	valid := writeConfig("valid.json", `{"networks":[
		{"id":"topaz","indexer":"https://indexer.topaz.example/graphql/query","rpc":"https://rpc.topaz.example"},
		{"id":"staging","indexer":"https://indexer.staging.example/graphql/query"}
	]}`)
	empty := writeConfig("empty.json", `{"networks":[]}`)
	dupes := writeConfig("dupes.json", `{"networks":[
		{"id":"topaz","indexer":"https://a.example/graphql/query"},
		{"id":"topaz","indexer":"https://b.example/graphql/query"}
	]}`)
	noIndexer := writeConfig("no-indexer.json", `{"networks":[{"id":"topaz"}]}`)
	malformed := writeConfig("malformed.json", `{"networks":`)

	tests := []struct {
		name       string
		configPath string
		networkID  string
		indexerURL string
		rpcURL     string
		wantErr    bool
		wantSource ConfigSource
		wantIDs    []string
	}{
		{
			// The regression this fix is about: -indexer alone used to be
			// silently dropped in favor of the built-in defaults.
			name:       "indexer flag alone is honored",
			indexerURL: "http://127.0.0.1:8546/graphql/query",
			wantSource: ConfigSourceFlags,
			wantIDs:    []string{defaultNetworkID},
		},
		{
			name:       "indexer and network flags",
			networkID:  "topaz",
			indexerURL: "http://127.0.0.1:8546/graphql/query",
			wantSource: ConfigSourceFlags,
			wantIDs:    []string{"topaz"},
		},
		{
			name:       "rpc flag is carried through",
			networkID:  "topaz",
			indexerURL: "http://127.0.0.1:8546/graphql/query",
			rpcURL:     "http://127.0.0.1:26657",
			wantSource: ConfigSourceFlags,
			wantIDs:    []string{"topaz"},
		},
		{
			name:      "network without indexer is an error",
			networkID: "topaz",
			wantErr:   true,
		},
		{
			name:    "rpc without indexer is an error",
			rpcURL:  "http://127.0.0.1:26657",
			wantErr: true,
		},
		{
			name:       "config file",
			configPath: valid,
			wantSource: ConfigSourceFile,
			wantIDs:    []string{"topaz", "staging"},
		},
		{
			name:       "config file combined with flags is an error",
			configPath: valid,
			indexerURL: "http://127.0.0.1:8546/graphql/query",
			wantErr:    true,
		},
		{
			name:       "config file with no networks is an error",
			configPath: empty,
			wantErr:    true,
		},
		{
			name:       "duplicate network ids are rejected",
			configPath: dupes,
			wantErr:    true,
		},
		{
			name:       "network without indexer url is rejected",
			configPath: noIndexer,
			wantErr:    true,
		},
		{
			// Previously this fell back to the defaults, so a typo in the
			// config file meant silently syncing a different chain.
			name:       "malformed config file is an error",
			configPath: malformed,
			wantErr:    true,
		},
		{
			name:       "missing config file is an error",
			configPath: filepath.Join(dir, "does-not-exist.json"),
			wantErr:    true,
		},
		{
			name:       "no flags falls back to defaults",
			wantSource: ConfigSourceDefault,
			wantIDs:    defaultConfig.IDs(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The no-flag case consults ./networks.json; keep the working
			// directory clean so it cannot pick up a stray file.
			t.Chdir(t.TempDir())

			cfg, source, err := ResolveConfig(tt.configPath, tt.networkID, tt.indexerURL, tt.rpcURL)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got config %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if source != tt.wantSource {
				t.Errorf("source = %q, want %q", source, tt.wantSource)
			}
			got := cfg.IDs()
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("network ids = %v, want %v", got, tt.wantIDs)
			}
			for i := range got {
				if got[i] != tt.wantIDs[i] {
					t.Fatalf("network ids = %v, want %v", got, tt.wantIDs)
				}
			}
			if tt.rpcURL != "" && cfg.Networks[0].RPCURL != tt.rpcURL {
				t.Errorf("rpc url = %q, want %q", cfg.Networks[0].RPCURL, tt.rpcURL)
			}
		})
	}
}

func TestResolveConfigImplicitFile(t *testing.T) {
	t.Chdir(t.TempDir())

	body := `{"networks":[{"id":"local","indexer":"http://127.0.0.1:8546/graphql/query"}]}`
	if err := os.WriteFile(implicitConfigFile, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", implicitConfigFile, err)
	}

	cfg, source, err := ResolveConfig("", "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source != ConfigSourceImplicit {
		t.Errorf("source = %q, want %q", source, ConfigSourceImplicit)
	}
	if ids := cfg.IDs(); len(ids) != 1 || ids[0] != "local" {
		t.Errorf("network ids = %v, want [local]", ids)
	}
}
