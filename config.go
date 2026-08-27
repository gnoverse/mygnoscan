package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

type NetworkConfig struct {
	ID         string `json:"id"`
	IndexerURL string `json:"indexer"`
	RPCURL     string `json:"rpc,omitempty"`
}

type AppConfig struct {
	Networks []NetworkConfig `json:"networks"`
}

// implicitConfigFile is picked up from the working directory when neither
// -config nor the single-network flags are given.
const implicitConfigFile = "networks.json"

// defaultNetworkID names the network when -indexer is passed without -network.
const defaultNetworkID = "default"

// The out-of-the-box networks: mainnet-ish plus the current public testnets.
// Testnets are named after gemstones and are retired as new ones launch — topaz
// was here and its endpoints no longer resolve — so this list needs revisiting
// whenever the next stone ships. A network that disappears degrades to a per-
// network circuit breaker rather than breaking startup.
var defaultConfig = &AppConfig{
	Networks: []NetworkConfig{
		{ID: "gnoland1", IndexerURL: "https://indexer.gno.land/graphql/query", RPCURL: "https://rpc.gno.land"},
		{ID: "pearl", IndexerURL: "https://indexer.pearl.testnets.gno.land/graphql/query", RPCURL: "https://rpc.pearl.testnets.gno.land"},
		{ID: "sapphire", IndexerURL: "https://indexer.sapphire.testnets.gno.land/graphql/query", RPCURL: "https://rpc.sapphire.testnets.gno.land"},
	},
}

// ConfigSource records where the effective config came from so startup can log it.
type ConfigSource string

const (
	ConfigSourceFile     ConfigSource = "config file"
	ConfigSourceFlags    ConfigSource = "command-line flags"
	ConfigSourceImplicit ConfigSource = implicitConfigFile + " in working directory"
	ConfigSourceDefault  ConfigSource = "built-in defaults"
)

func LoadConfig(path string) (*AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// ResolveConfig determines the effective network configuration from the flags.
//
// Flags that cannot be honored are reported as errors rather than dropped. A
// misconfigured instance still starts, still syncs and still looks healthy — it
// just serves a different chain than the operator asked for — so a silent
// fallback is close to undetectable in production.
func ResolveConfig(configPath, networkID, indexerURL, rpcURL string) (*AppConfig, ConfigSource, error) {
	cfg, source, err := resolveConfig(configPath, networkID, indexerURL, rpcURL)
	if err != nil {
		return nil, "", err
	}
	if err := cfg.validate(); err != nil {
		return nil, "", fmt.Errorf("%s: %w", source, err)
	}
	return cfg, source, nil
}

func resolveConfig(configPath, networkID, indexerURL, rpcURL string) (*AppConfig, ConfigSource, error) {
	single := networkID != "" || indexerURL != "" || rpcURL != ""

	switch {
	case configPath != "" && single:
		return nil, "", errors.New("-config cannot be combined with -network/-indexer/-rpc")

	case configPath != "":
		cfg, err := LoadConfig(configPath)
		if err != nil {
			return nil, "", err
		}
		return cfg, ConfigSourceFile, nil

	case single:
		// -indexer alone is enough: the network ID only labels the data.
		if indexerURL == "" {
			return nil, "", errors.New("-network and -rpc require -indexer to be set")
		}
		if networkID == "" {
			networkID = defaultNetworkID
		}
		return &AppConfig{Networks: []NetworkConfig{{
			ID:         networkID,
			IndexerURL: indexerURL,
			RPCURL:     rpcURL,
		}}}, ConfigSourceFlags, nil

	default:
		if _, err := os.Stat(implicitConfigFile); err == nil {
			cfg, err := LoadConfig(implicitConfigFile)
			if err != nil {
				return nil, "", err
			}
			return cfg, ConfigSourceImplicit, nil
		}
		return defaultConfig, ConfigSourceDefault, nil
	}
}

// validate rejects configs that would sync into the wrong place or not at all.
func (c *AppConfig) validate() error {
	if len(c.Networks) == 0 {
		return errors.New("no networks defined")
	}
	seen := make(map[string]bool, len(c.Networks))
	for i, n := range c.Networks {
		if n.ID == "" {
			return fmt.Errorf("network %d: missing id", i)
		}
		if n.IndexerURL == "" {
			return fmt.Errorf("network %q: missing indexer URL", n.ID)
		}
		// Duplicate IDs would give two syncers the same rows to write.
		if seen[n.ID] {
			return fmt.Errorf("duplicate network id %q", n.ID)
		}
		seen[n.ID] = true
	}
	return nil
}

// IDs returns the configured network IDs, for logging.
func (c *AppConfig) IDs() []string {
	ids := make([]string, 0, len(c.Networks))
	for _, n := range c.Networks {
		ids = append(ids, n.ID)
	}
	return ids
}
