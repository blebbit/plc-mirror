package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	_ "github.com/joho/godotenv/autoload"
	"github.com/kelseyhightower/envconfig"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"

	plcdb "github.com/blebbit/plc-mirror/pkg/db"
	plcrt "github.com/blebbit/plc-mirror/pkg/runtime"
)

type Config struct {
	LogFile     string
	LogFormat   string `default:"text"`
	LogLevel    int64  `default:"1"`
	MetricsPort string `split_words:"true"`
	DBUrl       string `envconfig:"POSTGRES_URL"`
	Upstream    string `default:"https://plc.directory"`
}

var config Config

func main() {
	flag.StringVar(&config.LogFile, "log", "", "Path to the log file. If empty, will log to stderr")
	flag.StringVar(&config.LogFormat, "log-format", "text", "Logging format. 'text' or 'json'")
	flag.Int64Var(&config.LogLevel, "log-level", 1, "Log level. -1 - trace, 0 - debug, 1 - info, 5 - panic")

	if err := envconfig.Process("plc", &config); err != nil {
		log.Fatalf("envconfig.Process: %s", err)
	}

	flag.Parse()

	ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	if err := runMain(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runMain(ctx context.Context) error {
	ctx = setupLogging(ctx)
	log := zerolog.Ctx(ctx)
	log.Debug().Msgf("Starting up...")

	// db setup
	db, err := plcdb.GetClient(config.DBUrl, ctx)
	if err != nil {
		return err
	}
	log.Debug().Msgf("DB connection established")

	err = plcdb.MigrateModels(db)
	if err != nil {
		return err
	}
	log.Debug().Msgf("DB schema updated")

	r, err := plcrt.NewRuntime(ctx, db)
	if err != nil {
		return fmt.Errorf("failed to create runtime: %w", err)
	}

	// setup the server routes
	http.HandleFunc("/ready", r.Ready)
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/info/{acct}", r.Info)
	http.HandleFunc("/{did}", r.DidDoc)

	// start mirror
	go r.StartMirror()

	// start server
	log.Info().Msgf("Starting HTTP listener on %q...", config.MetricsPort)
	srv := &http.Server{Addr: fmt.Sprintf(":%s", config.MetricsPort)}
	errCh := make(chan error)
	go func() {
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		if err := srv.Shutdown(context.Background()); err != nil {
			return fmt.Errorf("HTTP server shutdown failed: %w", err)
		}
	}
	return <-errCh
}

func setupLogging(ctx context.Context) context.Context {
	logFile := os.Stderr

	if config.LogFile != "" {
		f, err := os.OpenFile(config.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Fatalf("Failed to open the specified log file %q: %s", config.LogFile, err)
		}
		logFile = f
	}

	var output io.Writer

	switch config.LogFormat {
	case "json":
		output = logFile
	case "text":
		prefixList := []string{}
		info, ok := debug.ReadBuildInfo()
		if ok {
			prefixList = append(prefixList, info.Path+"/")
		}

		basedir := ""
		_, sourceFile, _, ok := runtime.Caller(0)
		if ok {
			basedir = filepath.Dir(sourceFile)
		}

		if basedir != "" && strings.HasPrefix(basedir, "/") {
			prefixList = append(prefixList, basedir+"/")
			head, _ := filepath.Split(basedir)
			for head != "/" {
				prefixList = append(prefixList, head)
				head, _ = filepath.Split(strings.TrimSuffix(head, "/"))
			}
		}

		output = zerolog.ConsoleWriter{
			Out:        logFile,
			NoColor:    true,
			TimeFormat: time.RFC3339,
			PartsOrder: []string{
				zerolog.LevelFieldName,
				zerolog.TimestampFieldName,
				zerolog.CallerFieldName,
				zerolog.MessageFieldName,
			},
			FormatFieldName:  func(i interface{}) string { return fmt.Sprintf("%s:", i) },
			FormatFieldValue: func(i interface{}) string { return fmt.Sprintf("%s", i) },
			FormatCaller: func(i interface{}) string {
				s := i.(string)
				for _, p := range prefixList {
					s = strings.TrimPrefix(s, p)
				}
				return s
			},
		}
	default:
		log.Fatalf("Invalid log format specified: %q", config.LogFormat)
	}

	logger := zerolog.New(output).Level(zerolog.Level(config.LogLevel)).With().Caller().Timestamp().Logger()

	ctx = logger.WithContext(ctx)

	zerolog.DefaultContextLogger = &logger
	log.SetOutput(logger)

	return ctx
}
