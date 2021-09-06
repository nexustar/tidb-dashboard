// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

// @title Dashboard API
// @version 1.0
// @license.name Apache 2.0
// @license.url http://www.apache.org/licenses/LICENSE-2.0.html
// @BasePath /dashboard/api
// @query.collection.format multi
// @securityDefinitions.apikey JwtAuth
// @in header
// @name Authorization

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof" // #nosec
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/pingcap/log"
	flag "github.com/spf13/pflag"
	"go.etcd.io/etcd/pkg/transport"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/pingcap/tidb-dashboard/pkg/apiserver"
	"github.com/pingcap/tidb-dashboard/pkg/config"
	keyvisualregion "github.com/pingcap/tidb-dashboard/pkg/keyvisual/region"
	"github.com/pingcap/tidb-dashboard/pkg/swaggerserver"
	"github.com/pingcap/tidb-dashboard/pkg/uiserver"
	"github.com/pingcap/tidb-dashboard/pkg/utils/version"
	_ "github.com/pingcap/tidb-dashboard/populate/distro"
)

type DashboardCLIConfig struct {
	ListenHost     string
	ListenPort     int
	EnableDebugLog bool
	CoreConfig     *config.Config
	// key-visual file mode for debug
	KVFileStartTime int64
	KVFileEndTime   int64
}

// NewCLIConfig generates the configuration of the dashboard in standalone mode.
func NewCLIConfig() *DashboardCLIConfig {
	cfg := &DashboardCLIConfig{}
	cfg.CoreConfig = config.Default()

	flag.StringVarP(&cfg.ListenHost, "host", "h", "127.0.0.1", "listen host of the Dashboard Server")
	flag.IntVarP(&cfg.ListenPort, "port", "p", 12333, "listen port of the Dashboard Server")
	flag.BoolVarP(&cfg.EnableDebugLog, "debug", "d", false, "enable debug logs")
	flag.StringVar(&cfg.CoreConfig.DataDir, "data-dir", cfg.CoreConfig.DataDir, "path to the Dashboard Server data directory")
	flag.StringVar(&cfg.CoreConfig.PublicPathPrefix, "path-prefix", cfg.CoreConfig.PublicPathPrefix, "public URL path prefix for reverse proxies")
	flag.StringVar(&cfg.CoreConfig.PDEndPoint, "pd", cfg.CoreConfig.PDEndPoint, "PD endpoint address that Dashboard Server connects to")
	flag.BoolVar(&cfg.CoreConfig.EnableTelemetry, "telemetry", cfg.CoreConfig.EnableTelemetry, "allow telemetry")
	flag.BoolVar(&cfg.CoreConfig.EnableExperimental, "experimental", cfg.CoreConfig.EnableExperimental, "allow experimental features")
	flag.BoolVar(&cfg.CoreConfig.EnableNonRootLogin, "non-root-login", cfg.CoreConfig.EnableNonRootLogin, "allow non root sql user login")

	showVersion := flag.BoolP("version", "v", false, "print version information and exit")

	clusterCaPath := flag.String("cluster-ca", "", "path of file that contains list of trusted SSL CAs")
	clusterCertPath := flag.String("cluster-cert", "", "path of file that contains X509 certificate in PEM format")
	clusterKeyPath := flag.String("cluster-key", "", "path of file that contains X509 key in PEM format")

	tidbCaPath := flag.String("tidb-ca", "", "path of file that contains list of trusted SSL CAs")
	tidbCertPath := flag.String("tidb-cert", "", "path of file that contains X509 certificate in PEM format")
	tidbKeyPath := flag.String("tidb-key", "", "path of file that contains X509 key in PEM format")

	// debug for keyvisual，hide help information
	flag.Int64Var(&cfg.KVFileStartTime, "keyviz-file-start", 0, "(debug) start time for file range in file mode")
	flag.Int64Var(&cfg.KVFileEndTime, "keyviz-file-end", 0, "(debug) end time for file range in file mode")
	_ = flag.CommandLine.MarkHidden("keyviz-file-start")
	_ = flag.CommandLine.MarkHidden("keyviz-file-end")

	flag.Parse()
	if *showVersion {
		version.PrintStandaloneModeInfo()
		_ = log.Sync()
		os.Exit(0)
	}

	cfg.CoreConfig.NormalizePublicPathPrefix()

	// setup TLS config for TiDB components
	if len(*clusterCaPath) != 0 && len(*clusterCertPath) != 0 && len(*clusterKeyPath) != 0 {
		cfg.CoreConfig.ClusterTLSConfig = buildTLSConfig(clusterCaPath, clusterKeyPath, clusterCertPath)
	}

	// setup TLS config for MySQL client
	// See https://github.com/pingcap/docs/blob/7a62321b3ce9318cbda8697503c920b2a01aeb3d/how-to/secure/enable-tls-clients.md#enable-authentication
	if (len(*tidbCertPath) != 0 && len(*tidbKeyPath) != 0) || len(*tidbCaPath) != 0 {
		cfg.CoreConfig.TiDBTLSConfig = buildTLSConfig(tidbCaPath, tidbKeyPath, tidbCertPath)
	}

	if err := cfg.CoreConfig.NormalizePDEndPoint(); err != nil {
		log.Fatal("Invalid PD Endpoint", zap.Error(err))
	}

	// keyvisual check
	startTime := cfg.KVFileStartTime
	endTime := cfg.KVFileEndTime
	if startTime != 0 || endTime != 0 {
		// file mode (debug)
		if startTime == 0 || endTime == 0 || startTime >= endTime {
			log.Fatal("keyviz-file-start must be smaller than keyviz-file-end, and none of them are 0")
		}
	}

	return cfg
}

func getContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		sc := make(chan os.Signal, 1)
		signal.Notify(sc,
			syscall.SIGHUP,
			syscall.SIGINT,
			syscall.SIGTERM,
			syscall.SIGQUIT)
		<-sc
		cancel()
	}()
	return ctx
}

func buildTLSConfig(caPath, keyPath, certPath *string) *tls.Config {
	tlsInfo := transport.TLSInfo{
		TrustedCAFile: *caPath,
		KeyFile:       *keyPath,
		CertFile:      *certPath,
	}
	tlsConfig, err := tlsInfo.ClientConfig()
	if err != nil {
		log.Fatal("Failed to load certificates", zap.Error(err))
	}
	return tlsConfig
}

func main() {
	// Flushing any buffered log entries
	defer log.Sync() //nolint:errcheck

	// init log will register the `pingcap-log` logfmt for
	_, _, err := log.InitLogger(&log.Config{})
	if err != nil {
		log.Fatal("failed to init log", zap.Error(err))
	}

	cliConfig := NewCLIConfig()
	ctx := getContext()

	if cliConfig.EnableDebugLog {
		log.SetLevel(zapcore.DebugLevel)
	}

	listenAddr := fmt.Sprintf("%s:%d", cliConfig.ListenHost, cliConfig.ListenPort)
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatal("TiDB Dashboard server listen failed", zap.String("addr", listenAddr), zap.Error(err))
	}

	var customKeyVisualProvider *keyvisualregion.DataProvider
	if cliConfig.KVFileStartTime > 0 {
		customKeyVisualProvider = &keyvisualregion.DataProvider{
			FileStartTime: cliConfig.KVFileStartTime,
			FileEndTime:   cliConfig.KVFileEndTime,
		}
	}
	assets := uiserver.Assets(cliConfig.CoreConfig)
	s := apiserver.NewService(
		cliConfig.CoreConfig,
		apiserver.StoppedHandler,
		assets,
		customKeyVisualProvider,
	)
	if err := s.Start(ctx); err != nil {
		log.Fatal("Can not start server", zap.Error(err))
	}
	defer s.Stop(context.Background()) //nolint:errcheck

	mux := http.DefaultServeMux
	uiHandler := http.StripPrefix(strings.TrimRight(config.UIPathPrefix, "/"), uiserver.Handler(assets))
	mux.Handle("/", http.RedirectHandler(config.UIPathPrefix, http.StatusFound))
	mux.Handle(config.UIPathPrefix, uiHandler)
	mux.Handle(config.APIPathPrefix, apiserver.Handler(s))
	mux.Handle(config.SwaggerPathPrefix, swaggerserver.Handler())

	log.Info(fmt.Sprintf("Dashboard server is listening at %s", listenAddr))
	log.Info(fmt.Sprintf("UI:      http://%s:%d/dashboard/", cliConfig.ListenHost, cliConfig.ListenPort))
	log.Info(fmt.Sprintf("API:     http://%s:%d/dashboard/api/", cliConfig.ListenHost, cliConfig.ListenPort))
	log.Info(fmt.Sprintf("Swagger: http://%s:%d/dashboard/api/swagger/", cliConfig.ListenHost, cliConfig.ListenPort))

	srv := &http.Server{Handler: mux}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		if err := srv.Serve(listener); err != http.ErrServerClosed {
			log.Error("Server aborted with an error", zap.Error(err))
		}
		wg.Done()
	}()

	<-ctx.Done()
	if err := srv.Shutdown(context.Background()); err != nil {
		log.Error("Can not stop server", zap.Error(err))
	}
	wg.Wait()
	log.Info("Stop dashboard server")
}
