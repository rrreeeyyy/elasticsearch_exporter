package main

import (
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"time"

	"context"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/justwatchcom/elasticsearch_exporter/collector"
	"github.com/justwatchcom/elasticsearch_exporter/pkg/clusterinfo"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	Name          = "elasticsearch_exporter"
	listenAddress = kingpin.Flag("web.listen-address",
		"Address to listen on for web interface and telemetry.").
		Default(":9114").Envar("WEB_LISTEN_ADDRESS").String()
	metricsPath = kingpin.Flag("web.telemetry-path",
		"Path under which to expose metrics.").
		Default("/metrics").Envar("WEB_TELEMETRY_PATH").String()
	esURI = kingpin.Flag("es.uri",
		"HTTP API address of an Elasticsearch node.").
		Default("http://localhost:9200").Envar("ES_URI").String()
	esTimeout = kingpin.Flag("es.timeout",
		"Timeout for trying to get stats from Elasticsearch.").
		Default("5s").Envar("ES_TIMEOUT").Duration()
	esAllNodes = kingpin.Flag("es.all",
		"Export stats for all nodes in the cluster. If used, this flag will override the flag es.node.").
		Default("false").Envar("ES_ALL").Bool()
	esNode = kingpin.Flag("es.node",
		"Node's name of which metrics should be exposed.").
		Default("_local").Envar("ES_NODE").String()
	esExportIndices = kingpin.Flag("es.indices",
		"Export stats for indices in the cluster.").
		Default("false").Envar("ES_INDICES").Bool()
	esExportIndicesSettings = kingpin.Flag("es.indices_settings",
		"Export stats for settings of all indices of the cluster.").
		Default("false").Envar("ES_INDICES_SETTINGS").Bool()
	esExportClusterSettings = kingpin.Flag("es.cluster_settings",
		"Export stats for cluster settings.").
		Default("false").Envar("ES_CLUSTER_SETTINGS").Bool()
	esExportShards = kingpin.Flag("es.shards",
		"Export stats for shards in the cluster (implies --es.indices).").
		Default("false").Envar("ES_SHARDS").Bool()
	esExportSnapshots = kingpin.Flag("es.snapshots",
		"Export stats for the cluster snapshots.").
		Default("false").Envar("ES_SNAPSHOTS").Bool()
	esClusterInfoInterval = kingpin.Flag("es.clusterinfo.interval",
		"Cluster info update interval for the cluster label").
		Default("5m").Envar("ES_CLUSTERINFO_INTERVAL").Duration()
	esCA = kingpin.Flag("es.ca",
		"Path to PEM file that contains trusted Certificate Authorities for the Elasticsearch connection.").
		Default("").Envar("ES_CA").String()
	esClientPrivateKey = kingpin.Flag("es.client-private-key",
		"Path to PEM file that contains the private key for client auth when connecting to Elasticsearch.").
		Default("").Envar("ES_CLIENT_PRIVATE_KEY").String()
	esClientCert = kingpin.Flag("es.client-cert",
		"Path to PEM file that contains the corresponding cert for the private key to connect to Elasticsearch.").
		Default("").Envar("ES_CLIENT_CERT").String()
	esInsecureSkipVerify = kingpin.Flag("es.ssl-skip-verify",
		"Skip SSL verification when connecting to Elasticsearch.").
		Default("false").Envar("ES_SSL_SKIP_VERIFY").Bool()
	logLevel = kingpin.Flag("log.level",
		"Sets the loglevel. Valid levels are debug, info, warn, error").
		Default("info").Envar("LOG_LEVEL").String()
	logFormat = kingpin.Flag("log.format",
		"Sets the log format. Valid formats are json and logfmt").
		Default("logfmt").Envar("LOG_FMT").String()
	logOutput = kingpin.Flag("log.output",
		"Sets the log output. Valid outputs are stdout and stderr").
		Default("stdout").Envar("LOG_OUTPUT").String()
)

func main() {
	kingpin.Version(version.Print(Name))
	kingpin.CommandLine.HelpFlag.Short('h')
	kingpin.Parse()

	logger := getLogger(*logLevel, *logOutput, *logFormat)

	// create a context that is cancelled on SIGKILL
	ctx, cancel := context.WithCancel(context.Background())

	// create a http server
	server := &http.Server{}

	handlerFunc := newPromHandler(ctx, logger)

	mux := http.DefaultServeMux
	mux.Handle(*metricsPath, promhttp.InstrumentMetricHandler(prometheus.DefaultRegisterer, handlerFunc))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte(`<html>
			<head><title>Elasticsearch Exporter</title></head>
			<body>
			<h1>Elasticsearch Exporter</h1>
			<p><a href="` + *metricsPath + `">Metrics</a></p>
			</body>
			</html>`))
		if err != nil {
			_ = level.Error(logger).Log(
				"msg", "failed handling writer",
				"err", err,
			)
		}
	})

	// health endpoint
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, http.StatusText(http.StatusOK), http.StatusOK)
	})

	server.Handler = mux
	server.Addr = *listenAddress

	_ = level.Info(logger).Log(
		"msg", "starting elasticsearch_exporter",
		"addr", *listenAddress,
	)

	go func() {
		if err := server.ListenAndServe(); err != nil {
			_ = level.Error(logger).Log(
				"msg", "http server quit",
				"err", err,
			)
			os.Exit(1)
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	// create a context for graceful http server shutdown
	srvCtx, srvCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer srvCancel()
	<-c
	_ = level.Info(logger).Log("msg", "shutting down")
	_ = server.Shutdown(srvCtx)
	cancel()
}

func newPromHandler(ctx context.Context, logger log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		registry := prometheus.NewRegistry()

		target := r.URL.Query().Get("target")
		if target != "" {
			esURI = &target
		}

		esURL, err := url.Parse(*esURI)
		if err != nil {
			_ = level.Error(logger).Log(
				"msg", "failed to parse es.uri",
				"err", err,
			)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("failed to parse es.uri or target"))
			return
		}

		// returns nil if not provided and falls back to simple TCP.
		tlsConfig := createTLSConfig(*esCA, *esClientCert, *esClientPrivateKey, *esInsecureSkipVerify)

		httpClient := &http.Client{
			Timeout: *esTimeout,
			Transport: &http.Transport{
				TLSClientConfig: tlsConfig,
				Proxy:           http.ProxyFromEnvironment,
			},
		}

		// version metric
		versionMetric := version.NewCollector(Name)
		registry.MustRegister(versionMetric)

		// cluster info retriever
		clusterInfoRetriever := clusterinfo.New(logger, httpClient, esURL, *esClusterInfoInterval)

		// start the cluster info retriever
		switch runErr := clusterInfoRetriever.Run(ctx); runErr {
		case nil:
			_ = level.Info(logger).Log(
				"msg", "started cluster info retriever",
				"interval", (*esClusterInfoInterval).String(),
			)
		case clusterinfo.ErrInitialCallTimeout:
			_ = level.Info(logger).Log("msg", "initial cluster info call timed out")
		default:
			_ = level.Error(logger).Log("msg", "failed to run cluster info retriever", "err", runErr)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("failed to run cluster info retriever"))
			return
		}

		// register cluster info retriever as prometheus collector
		registry.MustRegister(clusterInfoRetriever)

		registry.MustRegister(collector.NewClusterHealth(logger, httpClient, esURL))
		registry.MustRegister(collector.NewNodes(logger, httpClient, esURL, *esAllNodes, *esNode))

		if *esExportIndices || *esExportShards {
			iC := collector.NewIndices(logger, httpClient, esURL, *esExportShards)
			registry.MustRegister(iC)
			if registerErr := clusterInfoRetriever.RegisterConsumer(iC); registerErr != nil {
				_ = level.Error(logger).Log("msg", "failed to register indices collector in cluster info")
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte("failed to register indices collector in cluster info"))
				return
			}
		}

		if *esExportSnapshots {
			registry.MustRegister(collector.NewSnapshots(logger, httpClient, esURL))
		}

		if *esExportClusterSettings {
			registry.MustRegister(collector.NewClusterSettings(logger, httpClient, esURL))
		}

		if *esExportIndicesSettings {
			registry.MustRegister(collector.NewIndicesSettings(logger, httpClient, esURL))
		}

		gatherers := prometheus.Gatherers{
			prometheus.DefaultGatherer,
			registry,
		}
		// Delegate http serving to Prometheus client library, which will call collector.Collect.
		h := promhttp.HandlerFor(gatherers, promhttp.HandlerOpts{})
		h.ServeHTTP(w, r)
	}
}
