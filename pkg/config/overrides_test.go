package config

import (
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

const overridesYAML = `
clickhouse:
  dsn: http://base:8123/?async_insert=1
  params:
    database: base
    max_threads: "4"
insert:
  table: incoming
select:
  table_series: base_series
  table_samples: base_samples
  autocomplete_lookback: 12h
  series_partition_ms: 60000
  series_materialize_labels: [job, instance]
  samples_timestamp_uint32: false
override_insert:
  - when: GET["shard"] == "one"
    table: first
    clickhouse:
      params:
        database: first
  - when: GET["shard"] == "one" || HEADER["X-Region"] == "west"
    table: second
    id_func: name_with_sha256
    clickhouse:
      dsn: http://second:8123/
      params: {}
  - when: GET["shard"] == "three"
    clickhouse:
      dsn: http://third:8123/
override_series:
  - when: limit >= 100
    table: wide_series
    series_partition_ms: 120000
    series_materialized_labels: [team]
    clickhouse:
      params:
        database: wide
  - when: limit >= 10
    autocomplete_lookback: 1h
    clickhouse:
      dsn: http://ten:8123/
override_samples:
  - when: start < end && shard_index == 1
    table: samples_shard1
    samples_timestamp_uint32: true
    clickhouse:
      params:
        database: shard1
  - when: start < end
    table: samples_range
    clickhouse:
      dsn: http://range:8123/
`

func TestGetInsert(t *testing.T) {
	cfg, err := loadYAML(t, overridesYAML)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, path, region string
		wantTable, wantDSN string
		wantParams         map[string]string
	}{
		{"first match wins and inherits base DSN", "/?shard=one", "west", "first", "http://base:8123/?async_insert=1", map[string]string{"database": "first"}},
		{"second matches header", "/?shard=unknown", "west", "second", "http://second:8123/", map[string]string{}},
		{"third inherits base params", "/?shard=three", "", "incoming", "http://third:8123/", map[string]string{"database": "base", "max_threads": "4"}},
		{"no match returns base", "/?shard=other", "", "incoming", "http://base:8123/?async_insert=1", map[string]string{"database": "base", "max_threads": "4"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", tt.path, nil)
			if tt.region != "" {
				r.Header.Set("X-Region", tt.region)
			}
			got, err := cfg.GetInsert(NewEnvInsert().WithRequest(r))
			if err != nil {
				t.Fatal(err)
			}
			if got.Table != tt.wantTable || got.IDFunc != cfg.Insert.IDFunc || got.ClickHouse.DSN != tt.wantDSN || !reflect.DeepEqual(got.ClickHouse.Params, tt.wantParams) {
				t.Fatalf("GetInsert() = %+v, ClickHouse = %+v, want table %q, id_func %q, DSN %q, params %v", got, got.ClickHouse, tt.wantTable, cfg.Insert.IDFunc, tt.wantDSN, tt.wantParams)
			}
		})
	}
	if cfg.ClickHouse.DSN != "http://base:8123/?async_insert=1" || cfg.ClickHouse.Params["database"] != "base" {
		t.Fatalf("GetInsert mutated base ClickHouse: %+v", cfg.ClickHouse)
	}
}

func TestGetSeries(t *testing.T) {
	cfg, err := loadYAML(t, overridesYAML)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, table, dsn string
		limit            int
		lookback         time.Duration
		partition        int64
		labels           []string
		params           map[string]string
	}{
		{"first match wins", "wide_series", "http://base:8123/?async_insert=1", 100, 12 * time.Hour, 120000, []string{"team"}, map[string]string{"database": "wide"}},
		{"second inherits unspecified fields", "base_series", "http://ten:8123/", 10, time.Hour, 60000, []string{"job", "instance"}, map[string]string{"database": "base", "max_threads": "4"}},
		{"no match returns base", "base_series", "http://base:8123/?async_insert=1", 9, 12 * time.Hour, 60000, []string{"job", "instance"}, map[string]string{"database": "base", "max_threads": "4"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cfg.GetSeries(&EnvSeries{Limit: tt.limit})
			if err != nil {
				t.Fatal(err)
			}
			if got.Table != tt.table || got.AutocompleteLookback != tt.lookback || got.SeriesPartitionMs != tt.partition || !reflect.DeepEqual(got.SeriesMaterializedLabels, tt.labels) || got.ClickHouse.DSN != tt.dsn || !reflect.DeepEqual(got.ClickHouse.Params, tt.params) {
				t.Fatalf("GetSeries() = %+v, ClickHouse = %+v, want table %q, lookback %v, partition %d, labels %v, DSN %q, params %v", got, got.ClickHouse, tt.table, tt.lookback, tt.partition, tt.labels, tt.dsn, tt.params)
			}
		})
	}
}

func TestGetSamples(t *testing.T) {
	cfg, err := loadYAML(t, overridesYAML)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, table, dsn string
		start, end       int64
		shard            uint64
		timestampUInt32  bool
		params           map[string]string
	}{
		{"first match wins", "samples_shard1", "http://base:8123/?async_insert=1", 10, 20, 1, true, map[string]string{"database": "shard1"}},
		{"second inherits base params", "samples_range", "http://range:8123/", 10, 20, 0, false, map[string]string{"database": "base", "max_threads": "4"}},
		{"no match returns base", "base_samples", "http://base:8123/?async_insert=1", 20, 10, 1, false, map[string]string{"database": "base", "max_threads": "4"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cfg.GetSamples(&EnvSamples{Start: tt.start, End: tt.end, ShardIndex: tt.shard})
			if err != nil {
				t.Fatal(err)
			}
			if got.Table != tt.table || got.SamplesTimestampUInt32 != tt.timestampUInt32 || got.ClickHouse.DSN != tt.dsn || !reflect.DeepEqual(got.ClickHouse.Params, tt.params) {
				t.Fatalf("GetSamples() = %+v, ClickHouse = %+v, want table %q, timestamp_uint32 %v, DSN %q, params %v", got, got.ClickHouse, tt.table, tt.timestampUInt32, tt.dsn, tt.params)
			}
		})
	}
}

func TestNewEnvInsertWithRequest(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/v1/write?shard=one&shard=two&empty=", nil)
	r.Header.Set("X-Shard", "west")
	env := NewEnvInsert()
	if env.GetParams == nil || env.Headers == nil || env.WithRequest(r) != env {
		t.Fatalf("NewEnvInsert/WithRequest returned invalid env: %+v", env)
	}
	if !reflect.DeepEqual(env.GetParams, map[string]string{"shard": "one", "empty": ""}) || !reflect.DeepEqual(env.Headers, map[string]string{"X-Shard": "west"}) {
		t.Fatalf("WithRequest() = %+v", env)
	}
}

func TestShardingExample(t *testing.T) {
	cfg, err := LoadFromFile("../../example/sharding/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		shard, database string
	}{
		{shard: "1", database: "shard1"},
		{shard: "2", database: "shard2"},
		{shard: "other"},
	} {
		t.Run(tt.shard, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/v1/write?shard="+tt.shard, nil)
			got, err := cfg.GetInsert(NewEnvInsert().WithRequest(r))
			if err != nil {
				t.Fatal(err)
			}
			if got.Table != cfg.Insert.Table || got.IDFunc != cfg.Insert.IDFunc || got.ClickHouse.DSN != cfg.ClickHouse.DSN {
				t.Fatalf("GetInsert(shard=%s) lost base values: %+v, ClickHouse = %+v", tt.shard, got, got.ClickHouse)
			}
			if tt.database == "" {
				if got.ClickHouse.Params != nil {
					t.Fatalf("GetInsert(shard=%s) params = %v; want nil", tt.shard, got.ClickHouse.Params)
				}
			} else if got.ClickHouse.Params["database"] != tt.database {
				t.Fatalf("GetInsert(shard=%s) params = %v; want database %q", tt.shard, got.ClickHouse.Params, tt.database)
			}
		})
	}
}

func TestDownsamplingExample(t *testing.T) {
	cfg, err := LoadFromFile("../../example/downsampling/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for _, tt := range []struct {
		name       string
		start, end int64
		wantTable  string
	}{
		{name: "long range", start: now - 2*24*3600*1000, end: now, wantTable: "samples_1h"},
		{name: "old data", start: now - 10*24*3600*1000, end: now - 9*24*3600*1000, wantTable: "samples_1h"},
		{name: "short recent range", start: now - 60*1000, end: now, wantTable: cfg.Select.TableSamples},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cfg.GetSamples(&EnvSamples{Start: tt.start, End: tt.end})
			if err != nil {
				t.Fatal(err)
			}
			if got.Table != tt.wantTable || got.ClickHouse.DSN != cfg.ClickHouse.DSN {
				t.Fatalf("GetSamples() = %+v, ClickHouse = %+v; want table %q, DSN %q", got, got.ClickHouse, tt.wantTable, cfg.ClickHouse.DSN)
			}
		})
	}
}
