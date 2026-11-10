package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net/http/pprof"
	"os"
	"os/signal"
	"syscall"

	"github.com/lomik/pluto/pkg/config"
	"github.com/lomik/pluto/pkg/insert"
	"github.com/lomik/pluto/pkg/lg"
	"github.com/lomik/pluto/pkg/listen"
	"github.com/lomik/pluto/pkg/prom"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	var configFilename string
	var development bool
	flag.StringVar(&configFilename, "config", "/etc/pluto/config.yaml", "Config filename")
	flag.BoolVar(&development, "dev", false, "Use development config by default")
	flag.Parse()

	var logLevel = new(slog.LevelVar) // Info by default
	slog.SetDefault(
		slog.New(
			lg.NewHandler(
				slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}),
			),
		),
	)

	cfg, err := config.LoadFromFile(configFilename)
	if err != nil {
		log.Fatal(err)
	}

	// set log level from config
	logLevel.Set(cfg.Logging.Level)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpManager := listen.NewHTTP()
	if err := registerRoutes(ctx, cfg, httpManager); err != nil {
		log.Fatal(err)
	}

	if err := httpManager.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

func registerRoutes(ctx context.Context, cfg *config.Config, httpManager *listen.HTTP) error {
	// receiver
	if cfg.Insert.Enabled {
		slog.Info("insert enabled", slog.String("listen", cfg.Insert.Listen))
		mux := httpManager.Mux(cfg.Insert.Listen)
		rw := insert.NewPrometheusRemoteWrite(insert.Opts{
			Config: cfg,
		})

		mux.Handle("/api/v1/write", rw)
	}

	//debug
	if cfg.Debug.Enabled {
		slog.Info("debug enabled", slog.String("listen", cfg.Debug.Listen))
		mux := httpManager.Mux(cfg.Debug.Listen)

		if cfg.Debug.Metrics {
			prometheus.MustRegister(
				collectors.NewBuildInfoCollector(),
			)

			mux.Handle("/metrics", promhttp.HandlerFor(
				prometheus.DefaultGatherer, promhttp.HandlerOpts{
					Registry: prometheus.DefaultRegisterer,
				}))
		}

		if cfg.Debug.Pprof {
			mux.HandleFunc("/debug/pprof/", pprof.Index)
			mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
			mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
			mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
			mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		}

	}

	// prometheus
	if cfg.Prometheus.Enabled {
		slog.Info("prometheus enabled", slog.String("listen", cfg.Prometheus.Listen))
		mux := httpManager.Mux(cfg.Prometheus.Listen)

		p, err := prom.New(ctx, cfg)
		if err != nil {
			return err
		}

		p.Register(mux)
	}

	return nil
}
