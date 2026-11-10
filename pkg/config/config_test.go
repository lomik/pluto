package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadYAML(t *testing.T, content string) (*Config, error) {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(filename, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return LoadFromFile(filename)
}

func TestLoadFromFile(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{name: "defaults", yaml: "{}\n"},
		{name: "valid overrides", yaml: `
clickhouse:
  dsn: http://clickhouse:8123/?async_insert=1
insert:
  listen: localhost:9095
override_insert:
  - when: GET["shard"] == "one"
    table: samples_one
override_series:
  - when: limit > 100
    table: large_series
override_samples:
  - when: start < end
    table: samples_range
`},
		{name: "invalid yaml", yaml: "insert: [\n", wantErr: "yaml"},
		{name: "invalid override shape", yaml: "override_samples: [1]\n", wantErr: "cannot unmarshal"},
		{name: "invalid base dsn", yaml: "clickhouse:\n  dsn: not-a-uri\n", wantErr: "DSN"},
		{name: "empty base dsn", yaml: "clickhouse:\n  dsn: ''\n", wantErr: "required"},
		{name: "invalid listen", yaml: "insert:\n  listen: no-port\n", wantErr: "Listen"},
		{name: "invalid id_func", yaml: "insert:\n  id_func: unsupported\n", wantErr: "IDFunc"},
		{name: "invalid override dsn", yaml: "override_insert:\n  - clickhouse:\n      dsn: not-a-uri\n", wantErr: "DSN"},
		{name: "invalid series override dsn", yaml: "override_series:\n  - clickhouse:\n      dsn: not-a-uri\n", wantErr: "DSN"},
		{name: "invalid samples override dsn", yaml: "override_samples:\n  - clickhouse:\n      dsn: not-a-uri\n", wantErr: "DSN"},
		{name: "invalid override id_func", yaml: "override_insert:\n  - id_func: unsupported\n", wantErr: "IDFunc"},
		{name: "invalid insert expression", yaml: "override_insert:\n  - when: GET[\"shard\"] ==\n", wantErr: "unexpected"},
		{name: "invalid later insert expression", yaml: "override_insert:\n  - when: 'true'\n  - when: GET[\"shard\"] ==\n", wantErr: "unexpected"},
		{name: "invalid series expression", yaml: "override_series:\n  - when: unknown > 1\n", wantErr: "unknown"},
		{name: "invalid later series expression", yaml: "override_series:\n  - when: 'true'\n  - when: unknown > 1\n", wantErr: "unknown"},
		{name: "non-boolean samples expression", yaml: "override_samples:\n  - when: start + 1\n", wantErr: "bool"},
		{name: "invalid later samples expression", yaml: "override_samples:\n  - when: 'true'\n  - when: start + 1\n", wantErr: "bool"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadYAML(t, tt.yaml)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadFromFile() error = %v, config = %+v, want containing %q", err, cfg, tt.wantErr)
				}
				if cfg != nil {
					t.Fatalf("LoadFromFile() on error returned config: %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadFromFile() error = %v", err)
			}
			if cfg.ClickHouse.DSN == "" || cfg.Insert.Table != "samples_null" || cfg.Insert.IDFunc != "name_with_sha256" || cfg.Select.TableSeries != "series" || cfg.Select.TableSamples != "samples" {
				t.Fatalf("defaults not applied: %+v", cfg)
			}
			if cfg.Select.AutocompleteLookback != 168*time.Hour || cfg.Select.SeriesPartitionMs != 86400000 {
				t.Fatalf("select defaults not applied: %+v", cfg.Select)
			}
			if tt.name == "valid overrides" {
				if cfg.ClickHouse.DSN != "http://clickhouse:8123/?async_insert=1" || len(cfg.OverrideInsert) != 1 || len(cfg.OverrideSeries) != 1 || len(cfg.OverrideSamples) != 1 {
					t.Fatalf("explicit values/overrides not loaded: %+v", cfg)
				}
				if cfg.OverrideInsert[0].WhenExpr == nil || cfg.OverrideSeries[0].WhenExpr == nil || cfg.OverrideSamples[0].WhenExpr == nil {
					t.Fatalf("when expressions not compiled: %+v", cfg)
				}
			}
		})
	}
}

func TestLoadFromFileMissingFile(t *testing.T) {
	// configor silently ignores missing files without its own existence check.
	filename := filepath.Join(t.TempDir(), "missing.yaml")
	cfg, err := LoadFromFile(filename)
	if err == nil || !os.IsNotExist(err) || cfg != nil {
		t.Fatalf("LoadFromFile(%q) = %+v, %v; want nil, file-not-exist error", filename, cfg, err)
	}
}

func TestLoadRepositoryConfigs(t *testing.T) {
	for _, name := range []string{
		"config.yaml",
		"example/simple/config.yaml",
		"example/sharding/config.yaml",
		"example/downsampling/config.yaml",
	} {
		t.Run(name, func(t *testing.T) {
			filename := filepath.Join("..", "..", name)
			if _, err := LoadFromFile(filename); err != nil {
				t.Fatalf("LoadFromFile(%q): %v", filename, err)
			}
		})
	}
}

func TestValidateProgrammaticConfig(t *testing.T) {
	for _, tt := range []struct {
		name, dsn string
		wantErr   bool
	}{
		{name: "base DSN required", wantErr: true},
		{name: "invalid base DSN", dsn: "invalid", wantErr: true},
		{name: "valid base DSN", dsn: "http://localhost:8123/"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadYAML(t, "{}\n")
			if err != nil {
				t.Fatal(err)
			}
			cfg.ClickHouse.DSN = tt.dsn
			if err := cfg.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}
