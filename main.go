package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/uroshercog/k8s-secret-restart-controller/pkg/controller"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	apiserver  = flag.String("apiserver", "", "(optional) Kubernetes apiserver url.")
	kubeconfig = flag.String("kubeconfig", "", "Path to the kubeconfig file. Defaults to in-cluster config.")
	namespace  = flag.String("namespace", "", "Namespace to watch for claims.")

	syncPeriod = flag.Duration("sync-period", 0, "Sync all resources each period.")
)

func main() {
	flag.Parse()

	kconfig, err := clientcmd.BuildConfigFromFlags(*apiserver, *kubeconfig)
	if err != nil {
		panic(err.Error())
	}

	config := &controller.Config{
		Namespace:    *namespace,
		ResyncPeriod: *syncPeriod * time.Second,
	}

	ctrl, err := controller.New(config, kconfig)
	if err != nil {
		panic(err.Error())
	}

	stCh := make(chan struct{})
	go ctrl.Run(stCh)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigs:
		close(stCh)
	}
}
