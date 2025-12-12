/*
Copyright 2025 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/sirupsen/logrus"
	kwhlogrus "github.com/slok/kubewebhook/v2/pkg/log/logrus"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/images/webhooks/handlers"
)

type config struct {
	certFile string
	keyFile  string
}

func httpHandlerHealthz(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprint(w, "Ok.")
}

func initFlags() config {
	cfg := config{}

	fl := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	fl.StringVar(&cfg.certFile, "tls-cert-file", "", "TLS certificate file")
	fl.StringVar(&cfg.keyFile, "tls-key-file", "", "TLS key file")

	_ = fl.Parse(os.Args[1:])
	return cfg
}

const (
	port           = ":8443"
	MCRValidatorID = "MCRValidator"
)

func main() {
	logrusLogEntry := logrus.NewEntry(logrus.New())
	logrusLogEntry.Logger.SetLevel(logrus.DebugLevel)
	logger := kwhlogrus.NewLogrus(logrusLogEntry)

	cfg := initFlags()

	// Create Kubernetes client
	config, err := rest.InClusterConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting in-cluster config: %s", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating Kubernetes client: %s", err)
		os.Exit(1)
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating dynamic client: %s", err)
		os.Exit(1)
	}

	// Set the clients in handlers
	handlers.SetKubernetesClient(clientset)
	handlers.SetDynamicClient(dynClient)

	// Create validating webhook handler
	mcrValidatingWebhookHandler, err := handlers.GetValidatingWebhookHandler(
		handlers.MCRValidate,
		MCRValidatorID,
		&storagev1alpha1.ManifestCaptureRequest{},
		logger,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error creating mcrValidatingWebhookHandler: %s", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/validate", mcrValidatingWebhookHandler)
	mux.HandleFunc("/healthz", httpHandlerHealthz)

	logger.Infof("Listening on %s", port)
	err = http.ListenAndServeTLS(port, cfg.certFile, cfg.keyFile, mux)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error serving webhook: %s", err)
		os.Exit(1)
	}
}
