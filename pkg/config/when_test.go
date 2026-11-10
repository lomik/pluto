package config

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestConfigWhen(t *testing.T) {
	tests := []struct {
		name, expression, shard string
		want                    bool
		wantErr                 string
	}{
		{name: "empty expression does not match"},
		{name: "valid expression matches", expression: `GET["shard"] == "one"`, shard: "one", want: true},
		{name: "valid expression does not match", expression: `GET["shard"] == "one"`, shard: "two"},
		{name: "invalid syntax", expression: `GET["shard"] ==`, wantErr: "unexpected"},
		{name: "invalid type", expression: `GET["shard"] + 1`, wantErr: "invalid operation"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			when := &ConfigWhen{WhenStr: tt.expression}
			err := when.compileWhen(EnvInsert{})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("compileWhen() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if when.Pass() != (tt.expression == "") {
				t.Fatalf("Pass() = %v", when.Pass())
			}
			got, err := when.When(&EnvInsert{GetParams: map[string]string{"shard": tt.shard}})
			if err != nil || got != tt.want {
				t.Fatalf("When() = %v, %v; want %v, nil", got, err, tt.want)
			}
		})
	}
	var nilWhen *ConfigWhen
	if !nilWhen.Pass() {
		t.Fatal("nil ConfigWhen.Pass() = false")
	}
	if got, err := nilWhen.When(&EnvInsert{}); err != nil || got {
		t.Fatalf("nil ConfigWhen.When() = %v, %v; want false, nil", got, err)
	}
}

func TestCompileReplacesOldExpression(t *testing.T) {
	when := &ConfigWhen{WhenStr: "true"}
	if err := when.compileWhen(EnvSeries{}); err != nil || when.WhenExpr == nil {
		t.Fatalf("compileWhen(true) = %v, expr = %v", err, when.WhenExpr)
	}
	when.WhenStr = ""
	if err := when.compileWhen(EnvSeries{}); err != nil || when.WhenExpr != nil || !when.Pass() {
		t.Fatalf("compileWhen(empty) = %v, expr = %v, pass = %v", err, when.WhenExpr, when.Pass())
	}
}

func TestWhenRuntimeError(t *testing.T) {
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	when := &ConfigWhen{WhenStr: `GET["shard"] == "one"`}
	if err := when.compileWhen(EnvInsert{}); err != nil {
		t.Fatal(err)
	}
	if got, err := when.When(&EnvSeries{}); err == nil || got {
		t.Fatalf("When(incompatible env) = %v, %v; want false, error", got, err)
	}
}
