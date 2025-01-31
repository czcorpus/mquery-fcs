// Copyright 2023 Martin Zimandl <martin.zimandl@gmail.com>
// Copyright 2024 Tomas Machalek <tomas.machalek@gmail.com>
// Copyright 2023 Institute of the Czech National Corpus,
//                Faculty of Arts, Charles University
//   This file is part of MQUERY.
//
//  MQUERY is free software: you can redistribute it and/or modify
//  it under the terms of the GNU General Public License as published by
//  the Free Software Foundation, either version 3 of the License, or
//  (at your option) any later version.
//
//  MQUERY is distributed in the hope that it will be useful,
//  but WITHOUT ANY WARRANTY; without even the implied warranty of
//  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//  GNU General Public License for more details.
//
//  You should have received a copy of the GNU General Public License
//  along with MQUERY.  If not, see <https://www.gnu.org/licenses/>.

//go:generate pigeon -o ../../query/parser/fcsql/fcsql.go ../../query/parser/fcsql/fcsql.peg
//go:generate pigeon -o ../../query/parser/basic/basic.go ../../query/parser/basic/basic.peg

package main

import (
	"context"
	"encoding/gob"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/czcorpus/cnc-gokit/collections"
	"github.com/czcorpus/cnc-gokit/logging"
	"github.com/czcorpus/cnc-gokit/uniresp"
	"github.com/czcorpus/mquery-common/concordance"
	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"

	"github.com/czcorpus/mquery-sru/cnf"
	"github.com/czcorpus/mquery-sru/corpus"
	"github.com/czcorpus/mquery-sru/general"
	"github.com/czcorpus/mquery-sru/handler"
	"github.com/czcorpus/mquery-sru/handler/form"
	"github.com/czcorpus/mquery-sru/monitoring"
	"github.com/czcorpus/mquery-sru/rdb"
	"github.com/czcorpus/mquery-sru/worker"
)

var (
	version   string
	buildDate string
	gitCommit string
)

func getEnv(name string) string {
	for _, p := range os.Environ() {
		items := strings.Split(p, "=")
		if len(items) == 2 && items[0] == name {
			return items[1]
		}
	}
	return ""
}

func init() {
	gob.Register(&concordance.Token{})
	gob.Register(&concordance.Struct{})
	gob.Register(&concordance.CloseStruct{})
}

func watchdogIdentificationMiddleware(WatchdogReqFilterConf *cnf.WatchdogReqFilter) gin.HandlerFunc {
	return func(c *gin.Context) {
		logging.AddLogEvent(
			c,
			"isWatchdogQuery",
			WatchdogReqFilterConf != nil && c.GetHeader(WatchdogReqFilterConf.HTTPIdHeaderName) == WatchdogReqFilterConf.HTTPIdHeaderToken,
		)
		c.Next()
	}
}

func runApiServer(
	ctx context.Context,
	conf *cnf.Conf,
	radapter *rdb.Adapter,
) {
	log.Info().Msg("Starting MQuery-SRU server")
	if !conf.Logging.Level.IsDebugMode() {
		gin.SetMode(gin.ReleaseMode)
	}

	engine := gin.New()
	engine.ForwardedByClientIP = true
	if len(conf.TrustedProxies) > 0 {
		if err := engine.SetTrustedProxies(conf.TrustedProxies); err != nil {
			log.Error().Err(err).Msg("Failed to set trusted proxies")
			return
		}
	}
	engine.Use(gin.Recovery())
	engine.Use(logging.GinMiddleware())
	engine.Use(watchdogIdentificationMiddleware(conf.WatchdogReqFilter))
	engine.NoMethod(uniresp.NoMethodHandler)
	engine.NoRoute(uniresp.NotFoundHandler)

	FCSActions := handler.NewFCSHandler(conf.ServerInfo, conf.CorporaSetup, radapter)
	engine.GET("/", FCSActions.FCSHandler)
	engine.HEAD("/", FCSActions.FCSHandler)

	viewHandler := handler.NewViewHandler(FCSActions, conf.AssetsURLPath)
	engine.GET("/ui/view", viewHandler.Handle)

	engine.StaticFS(
		"/ui/assets",
		gin.Dir(filepath.Join(conf.SourcesRootDir, "assets"), false),
	)

	uIActions := form.NewFormHandler(
		conf.ServerInfo, conf.CorporaSetup, conf.SourcesRootDir)
	engine.GET("/ui/form", uIActions.Handle)

	logger := monitoring.NewWorkerJobLogger(conf.TimezoneLocation())
	logger.GoRunTimelineWriter()

	monitoringActions := monitoring.NewActions(logger, conf.TimezoneLocation())
	engine.GET("/monitoring/workers-load", monitoringActions.WorkersLoad)

	srv := &http.Server{
		Handler:      engine,
		Addr:         fmt.Sprintf("%s:%d", conf.ListenAddress, conf.ListenPort),
		WriteTimeout: time.Duration(conf.ServerWriteTimeoutSecs) * time.Second,
		ReadTimeout:  time.Duration(conf.ServerReadTimeoutSecs) * time.Second,
	}

	srvErrChan := make(chan error, 1)

	go func() {
		log.Info().Msgf("listening at %s:%d", conf.ListenAddress, conf.ListenPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErrChan <- err
		}
	}()

	select {
	case err := <-srvErrChan:
		log.Error().Err(err).Msg("Server error")
	case <-ctx.Done():
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := srv.Shutdown(ctx)
		if err != nil {
			log.Info().Err(err).Msg("Shutdown request error")
		}
	}
}

func runWorker(ctx context.Context, conf *cnf.Conf, workerID string, radapter *rdb.Adapter) {
	log.Info().Msg("Starting MQuery-SRU worker")
	ch := radapter.Subscribe()
	logger := monitoring.NewWorkerJobLogger(conf.TimezoneLocation())
	w := worker.NewWorker(ctx, workerID, radapter, ch, logger)
	w.Listen()
}

func getWorkerID() (workerID string) {
	workerID = getEnv("WORKER_ID")
	if workerID == "" {
		workerID = "0"
	}
	return
}

func main() {
	version := general.VersionInfo{
		Version:   version,
		BuildDate: buildDate,
		GitCommit: gitCommit,
	}

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "MQuery-SRU - A Manatee-open based SRU endpoint.\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n\t%s [options] server [config.json]\n\t", filepath.Base(os.Args[0]))
		fmt.Fprintf(os.Stderr, "Usage:\n\t%s [options] worker [config.json]\n\t", filepath.Base(os.Args[0]))
		fmt.Fprintf(os.Stderr, "Usage:\n\t%s translate [basic/advanced]\n\t", filepath.Base(os.Args[0]))
		fmt.Fprintf(os.Stderr, "%s [options] version\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
	flag.Parse()
	action := flag.Arg(0)
	switch action {
	case "version":
		fmt.Printf("MQuery-SRU %s\nbuild date: %s\nlast commit: %s\n", version.Version, version.BuildDate, version.GitCommit)
		return
	case "translate":
		switch flag.Arg(1) {
		case "basic":
			repl(translateBasicQuery)
		case "advanced":
			repl(translateFCSQuery)
		default:
			fmt.Println("Unknown query type")
			os.Exit(2)
		}
	}

	conf := cnf.LoadConfig(flag.Arg(1))

	if action == "worker" {
		if conf.Logging.Path != "" {
			conf.Logging.Path = filepath.Join(filepath.Dir(conf.Logging.Path), "worker.log")
		}
		logging.SetupLogging(conf.Logging)
		log.Logger = log.Logger.With().Str("worker", getWorkerID()).Logger()

	} else if action == "test" {
		cnf.ValidateAndDefaults(conf)
		log.Info().Msg("config OK")
		return

	} else {
		logging.SetupLogging(conf.Logging)
	}
	log.Info().Msg("MQuery-SRU initialization...")
	cnf.ValidateAndDefaults(conf)

	log.Info().
		Strs(
			"corpora",
			collections.SliceMap(
				conf.CorporaSetup.Resources,
				func(item *corpus.CorpusSetup, i int) string { return item.ID },
			),
		).
		Msgf("providing %d resources/corpora", len(conf.CorporaSetup.Resources))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	radapter := rdb.NewAdapter(ctx, conf.Redis)

	switch action {
	case "server":
		err := radapter.TestConnection(50*time.Second, 10*time.Second)
		if err != nil {
			log.Fatal().Err(err).Msg("failed to connect to Redis")
		}
		runApiServer(ctx, conf, radapter)
	case "worker":
		err := radapter.TestConnection(50*time.Second, 10*time.Second)
		if err != nil {
			log.Fatal().Err(err).Msg("failed to connect to Redis")
		}
		runWorker(ctx, conf, getWorkerID(), radapter)
	default:
		log.Fatal().Msgf("Unknown action %s", action)
	}

}
