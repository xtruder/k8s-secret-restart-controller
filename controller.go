package main

import (
	"os"
	"fmt"
	"time"
	"sync"
	"syscall"
	"os/signal"
	"io/ioutil"
	coreV1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/kubernetes"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
)

type config struct {
	namespace     string
	bearer        string
	host          string
	resyncTimeout time.Duration
}

type controller struct {
	mx sync.Mutex

	cfg config

	cs *kubernetes.Clientset

	stCh <-chan struct{}

	processPods chan string
	pendingPods []string
}

func (c *controller) Run(stChan <-chan struct{}) {
	config := &rest.Config{
		Host:            c.cfg.host,
		BearerToken:     c.cfg.bearer,
		TLSClientConfig: rest.TLSClientConfig{Insecure: true},
	}

	c.stCh = stChan
	c.cs = kubernetes.NewForConfigOrDie(config)

	go c.restartPods()
	go c.monitorSecrets()
	go c.monitorPods()

	go func() {
		select {
		case <-c.stCh:
			close(c.processPods)
		}
	}()
}

func (c controller) restartPods() {
	for {
		select {
		case podName := <-c.processPods:
			pod, err := c.cs.CoreV1().Pods(c.cfg.namespace).Get(podName, v1.GetOptions{})
			if err != nil {
				fmt.Printf("could not load pod: %s\n", podName)
				// TODO: How to handle this properly?
				continue
			}

			if pod.Status.Phase != coreV1.PodRunning {
				fmt.Printf("not restarting pod %s: not in running phase\n", pod.Name)
				// TODO: How to handle this properly?
				continue
			}

			/*if err := c.cs.CoreV1().Pods(c.cfg.namespace).Delete(pod.Name, &v1.DeleteOptions{}); err != nil {
				fmt.Printf("pod not restarted: %s\n", err.Error())
				// TODO: How to handle this properly?
			}*/
		}
	}
}

func (c controller) monitorSecrets() {
	lw := cache.NewListWatchFromClient(c.cs.CoreV1().RESTClient(), "secrets", c.cfg.namespace, fields.Everything())

	_, ctrl := cache.NewInformer(lw, &coreV1.Secret{}, c.cfg.resyncTimeout, cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			// Check if the value of the secret changed
			oldSecret := oldObj.(*coreV1.Secret)
			newSecret := newObj.(*coreV1.Secret)

			if oldSecret == newSecret {
				// Secrets are the same, just continue
				return
			}

			fmt.Printf("secret: %#v\n", newSecret.Name)

			pods, err := c.cs.CoreV1().Pods(c.cfg.namespace).List(v1.ListOptions{})
			if err != nil {
				fmt.Printf("error fetching pods: " + err.Error())
				return
			}

			var ps []string

			// Find the pods using this secret
			for _, p := range pods.Items {
				// If at least one of the containers is using the secret then restart the whole pod
				for _, cn := range p.Spec.Containers {
					for _, s := range cn.Env {
						if s.ValueFrom != nil && s.ValueFrom.SecretKeyRef != nil && s.ValueFrom.SecretKeyRef.Name == newSecret.Name {
							// A container in this pod is using a secret that just got updated
							// We can restart the pod

							// If the pod is running then restart it. If the pod is pending then store the event and process
							// it once the state of the pod changes to running, then check if the secrets match. If they
							// don't then restart it.
							fmt.Printf("pod %s:%s has secret %s\n", p.Name, cn.Name, s.Name)

							// If pod was created before the secret then we have to restart the pod
							if p.CreationTimestamp.Time.Before(newSecret.CreationTimestamp.Time) {
								// Restart
								if err := c.cs.CoreV1().Pods(c.cfg.namespace).Delete(p.Name, &v1.DeleteOptions{}); err != nil {
									fmt.Printf("error restarting pod %s: %s\n", p.Name, err.Error())
									continue
								}
							}

							ps = append(ps, cn.Name)
						}
					}
				}
			}

			// Deduplicate and push to pod process queue
			for _, v := range unique(ps) {
				c.processPods <- v
			}
		},
	})

	ctrl.Run(c.stCh)
}

func (c controller) monitorPods() {
	lw := cache.NewListWatchFromClient(c.cs.CoreV1().RESTClient(), "pods", c.cfg.namespace, fields.Everything())

	_, ctrl := cache.NewInformer(lw, &coreV1.Pod{}, c.cfg.resyncTimeout, cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldPod := oldObj.(*coreV1.Pod)
			newPod := newObj.(*coreV1.Pod)

			// If pods state changes to running then queue all of the pending secrets
			if oldPod.Status.Phase == coreV1.PodPending && newPod.Status.Phase == coreV1.PodRunning {
				c.mx.Lock()
				defer c.mx.Unlock()

				idx := find(c.pendingPods, newPod.Name)
				if idx > -1 {
					c.pendingPods = append(c.pendingPods[:idx], c.pendingPods[idx+1:]...)
				}
			}

			c.processPods <- newPod.Name
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(coreV1.Pod)

			c.mx.Lock()
			defer c.mx.Unlock()

			idx := find(c.pendingPods, pod.Name)
			if idx > -1 {
				c.pendingPods = append(c.pendingPods[:idx], c.pendingPods[idx+1:]...)
			}
		},
	})

	ctrl.Run(c.stCh)
}

func newController(cfg config) *controller {
	//TODO: Should the clientset be initialized here?
	return &controller{
		cfg:         cfg,
		processPods: make(chan string),
		pendingPods: make([]string, 10),
	}
}
func stringFromEnvOrPanic(key string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	panic(fmt.Errorf("missing %s environment variable", key))
}

func stringFromFileOrPanic(file string) string {
	d, err := ioutil.ReadFile(file)
	if err != nil {
		panic(fmt.Errorf("could not load string from file %s: %s", file, err.Error()))
	}
	return string(d)
}

func unique(arr []string) []string {
	m := make(map[string]int)

	for _, v := range arr {
		m[v]++
	}

	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func find(arr []string, fv string) int {
	idx := -1

	for i, v := range arr {
		if v == fv {
			idx = i
			break
		}
	}

	return idx
}

func main() {
	ctrl := newController(config{
		namespace:     stringFromFileOrPanic("/var/run/secrets/kubernetes.io/serviceaccount/namespace"),
		host:          stringFromEnvOrPanic("KUBERNETES_SERVICE_HOST"),
		resyncTimeout: 5 * time.Second,
		bearer:        stringFromFileOrPanic("/var/run/secrets/kubernetes.io/serviceaccount/token"),
	})

	stCh := make(chan struct{})
	go ctrl.Run(stCh)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigs:
		close(stCh)
	}
}
