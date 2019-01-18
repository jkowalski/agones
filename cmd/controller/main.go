// Copyright 2018 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Controller for gameservers
package main

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"agones.dev/agones/pkg"
	"agones.dev/agones/pkg/client/clientset/versioned"
	"agones.dev/agones/pkg/client/informers/externalversions"
	"agones.dev/agones/pkg/fleetallocation"
	"agones.dev/agones/pkg/fleetautoscalers"
	"agones.dev/agones/pkg/fleets"
	"agones.dev/agones/pkg/gameserverallocations"
	"agones.dev/agones/pkg/gameservers"
	"agones.dev/agones/pkg/gameserversets"
	"agones.dev/agones/pkg/metrics"
	"agones.dev/agones/pkg/util/runtime"
	"agones.dev/agones/pkg/util/signals"
	"agones.dev/agones/pkg/util/webhooks"
	"github.com/heptiolabs/healthcheck"
	"github.com/pkg/errors"
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	extclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	enableMetricsFlag     = "metrics"
	sidecarImageFlag      = "sidecar-image"
	sidecarCPURequestFlag = "sidecar-cpu-request"
	sidecarCPULimitFlag   = "sidecar-cpu-limit"
	pullSidecarFlag       = "always-pull-sidecar"
	minPortFlag           = "min-port"
	maxPortFlag           = "max-port"
	certFileFlag          = "cert-file"
	keyFileFlag           = "key-file"
	kubeconfigFlag        = "kubeconfig"
	workers               = 64
	defaultResync         = 30 * time.Second
)

var (
	logger = runtime.NewLoggerWithSource("main")
)

// main starts the operator for the gameserver CRD
func main() {
	ctlConf := parseEnvFlags()

	// logrus.SetFormatter(&logrus.TextFormatter{
	// 	FullTimestamp:   true,
	// 	TimestampFormat: "2006-01-02 15:04:05.000000",
	// 	//ForceColors:     true,
	// })

	logger.WithField("version", pkg.Version).
		WithField("ctlConf", ctlConf).Info("starting gameServer operator...")

	if err := ctlConf.validate(); err != nil {
		logger.WithError(err).Fatal("Could not create controller from environment or flags")
	}

	// if the kubeconfig fails BuildConfigFromFlags will try in cluster config
	clientConf, err := clientcmd.BuildConfigFromFlags("", ctlConf.KubeConfig)
	if err != nil {
		logger.WithError(err).Fatal("Could not create in cluster config")
	}

	clientConf.QPS = 100
	clientConf.Burst = 200

	kubeClient, err := kubernetes.NewForConfig(clientConf)
	if err != nil {
		logger.WithError(err).Fatal("Could not create the kubernetes clientset")
	}

	extClient, err := extclientset.NewForConfig(clientConf)
	if err != nil {
		logger.WithError(err).Fatal("Could not create the api extension clientset")
	}

	agonesClient, err := versioned.NewForConfig(clientConf)
	if err != nil {
		logger.WithError(err).Fatal("Could not create the agones api clientset")
	}

	wh := webhooks.NewWebHook(ctlConf.CertFile, ctlConf.KeyFile)
	agonesInformerFactory := externalversions.NewSharedInformerFactory(agonesClient, defaultResync)
	kubeInformationFactory := informers.NewSharedInformerFactory(kubeClient, defaultResync)

	server := &httpServer{}
	var health healthcheck.Handler
	var metricsController *metrics.Controller

	if ctlConf.Metrics {
		registry := prom.NewRegistry()
		metricHandler, err := metrics.RegisterPrometheusExporter(registry)
		if err != nil {
			logger.WithError(err).Fatal("Could not create register prometheus exporter")
		}
		server.Handle("/metrics", metricHandler)
		health = healthcheck.NewMetricsHandler(registry, "agones")
		metricsController = metrics.NewController(kubeClient, agonesClient, agonesInformerFactory)

	} else {
		health = healthcheck.NewHandler()
	}

	server.Handle("/", health)

	allocationMutex := &sync.Mutex{}

	gsController := gameservers.NewController(wh, health, allocationMutex,
		ctlConf.MinPort, ctlConf.MaxPort, ctlConf.SidecarImage, ctlConf.AlwaysPullSidecar,
		ctlConf.SidecarCPURequest, ctlConf.SidecarCPULimit,
		kubeClient, kubeInformationFactory, extClient, agonesClient, agonesInformerFactory)
	gsSetController := gameserversets.NewController(wh, health, allocationMutex,
		kubeClient, extClient, agonesClient, agonesInformerFactory)
	fleetController := fleets.NewController(wh, health, kubeClient, extClient, agonesClient, agonesInformerFactory)
	faController := fleetallocation.NewController(wh, allocationMutex,
		kubeClient, extClient, agonesClient, agonesInformerFactory)
	gasController := gameserverallocations.NewController(wh, health, allocationMutex, kubeClient,
		kubeInformationFactory, extClient, agonesClient, agonesInformerFactory)
	fasController := fleetautoscalers.NewController(wh, health,
		kubeClient, extClient, agonesClient, agonesInformerFactory)

	rs := []runner{
		wh, gsController, gsSetController, fleetController, faController, fasController, metricsController, gasController, server,
	}

	stop := signals.NewStopChannel()

	kubeInformationFactory.Start(stop)
	agonesInformerFactory.Start(stop)

	for _, r := range rs {
		if r == nil {
			continue
		}
		go func(rr runner) {
			if runErr := rr.Run(workers, stop); runErr != nil {
				logger.WithError(runErr).Fatalf("could not start runner: %s", reflect.TypeOf(rr))
			}
		}(r)
	}

	<-stop
	logger.Info("Shut down agones controllers")
}

func parseEnvFlags() config {
	exec, err := os.Executable()
	if err != nil {
		logger.WithError(err).Fatal("Could not get executable path")
	}

	base := filepath.Dir(exec)
	viper.SetDefault(sidecarImageFlag, "gcr.io/agones-images/agones-sdk:"+pkg.Version)
	viper.SetDefault(sidecarCPURequestFlag, "0")
	viper.SetDefault(sidecarCPULimitFlag, "0")
	viper.SetDefault(pullSidecarFlag, false)
	viper.SetDefault(certFileFlag, filepath.Join(base, "certs/server.crt"))
	viper.SetDefault(keyFileFlag, filepath.Join(base, "certs/server.key"))
	viper.SetDefault(enableMetricsFlag, true)

	pflag.String(sidecarImageFlag, viper.GetString(sidecarImageFlag), "Flag to overwrite the GameServer sidecar image that is used. Can also use SIDECAR env variable")
	pflag.String(sidecarCPULimitFlag, viper.GetString(sidecarCPULimitFlag), "Flag to overwrite the GameServer sidecar container's cpu limit. Can also use SIDECAR_CPU_LIMIT env variable")
	pflag.String(sidecarCPURequestFlag, viper.GetString(sidecarCPURequestFlag), "Flag to overwrite the GameServer sidecar container's cpu request. Can also use SIDECAR_CPU_REQUEST env variable")
	pflag.Bool(pullSidecarFlag, viper.GetBool(pullSidecarFlag), "For development purposes, set the sidecar image to have a ImagePullPolicy of Always. Can also use ALWAYS_PULL_SIDECAR env variable")
	pflag.Int32(minPortFlag, 0, "Required. The minimum port that that a GameServer can be allocated to. Can also use MIN_PORT env variable.")
	pflag.Int32(maxPortFlag, 0, "Required. The maximum port that that a GameServer can be allocated to. Can also use MAX_PORT env variable")
	pflag.String(keyFileFlag, viper.GetString(keyFileFlag), "Optional. Path to the key file")
	pflag.String(certFileFlag, viper.GetString(certFileFlag), "Optional. Path to the crt file")
	pflag.String(kubeconfigFlag, viper.GetString(kubeconfigFlag), "Optional. kubeconfig to run the controller out of the cluster. Only use it for debugging as webhook won't works.")
	pflag.Bool(enableMetricsFlag, viper.GetBool(enableMetricsFlag), "Flag to activate metrics of Agones. Can also use METRICS env variable.")
	pflag.Parse()

	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	runtime.Must(viper.BindEnv(sidecarImageFlag))
	runtime.Must(viper.BindEnv(sidecarCPULimitFlag))
	runtime.Must(viper.BindEnv(sidecarCPURequestFlag))
	runtime.Must(viper.BindEnv(pullSidecarFlag))
	runtime.Must(viper.BindEnv(minPortFlag))
	runtime.Must(viper.BindEnv(maxPortFlag))
	runtime.Must(viper.BindEnv(keyFileFlag))
	runtime.Must(viper.BindEnv(certFileFlag))
	runtime.Must(viper.BindEnv(kubeconfigFlag))
	runtime.Must(viper.BindEnv(enableMetricsFlag))
	runtime.Must(viper.BindPFlags(pflag.CommandLine))

	request, err := resource.ParseQuantity(viper.GetString(sidecarCPURequestFlag))
	if err != nil {
		logger.WithError(err).Fatalf("could not parse %s", sidecarCPURequestFlag)
	}

	limit, err := resource.ParseQuantity(viper.GetString(sidecarCPULimitFlag))
	if err != nil {
		logger.WithError(err).Fatalf("could not parse %s", sidecarCPULimitFlag)
	}

	return config{
		MinPort:           int32(viper.GetInt64(minPortFlag)),
		MaxPort:           int32(viper.GetInt64(maxPortFlag)),
		SidecarImage:      viper.GetString(sidecarImageFlag),
		SidecarCPURequest: request,
		SidecarCPULimit:   limit,
		AlwaysPullSidecar: viper.GetBool(pullSidecarFlag),
		KeyFile:           viper.GetString(keyFileFlag),
		CertFile:          viper.GetString(certFileFlag),
		KubeConfig:        viper.GetString(kubeconfigFlag),
		Metrics:           viper.GetBool(enableMetricsFlag),
	}
}

// config stores all required configuration to create a game server controller.
type config struct {
	MinPort           int32
	MaxPort           int32
	SidecarImage      string
	SidecarCPURequest resource.Quantity
	SidecarCPULimit   resource.Quantity
	AlwaysPullSidecar bool
	Metrics           bool
	KeyFile           string
	CertFile          string
	KubeConfig        string
}

// validate ensures the ctlConfig data is valid.
func (c config) validate() error {
	if c.MinPort <= 0 || c.MaxPort <= 0 {
		return errors.New("min Port and Max Port values are required")
	}
	if c.MaxPort < c.MinPort {
		return errors.New("max Port cannot be set less that the Min Port")
	}
	return nil
}

type runner interface {
	Run(workers int, stop <-chan struct{}) error
}

type httpServer struct {
	http.ServeMux
}

func (h *httpServer) Run(workers int, stop <-chan struct{}) error {
	logger.Info("Starting http server...")
	srv := &http.Server{
		Addr:    ":8080",
		Handler: h,
	}
	defer srv.Close() // nolint: errcheck

	if err := srv.ListenAndServe(); err != nil {
		if err == http.ErrServerClosed {
			logger.WithError(err).Info("http server closed")
		} else {
			wrappedErr := errors.Wrap(err, "Could not listen on :8080")
			runtime.HandleError(logger.WithError(wrappedErr), wrappedErr)
		}
	}
	return nil
}
