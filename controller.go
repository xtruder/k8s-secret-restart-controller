package main

import (
	"os"
	"fmt"
	"time"
	"sync"
	"syscall"
	"strings"
	"os/signal"
	"io/ioutil"
	coreV1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/kubernetes"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/api/policy/v1beta1"
)

var (
	ErrDisruptionBudget = "Cannot evict pod as it would violate the pod's disruption budget."
)

type podChangeHandler func()

type config struct {
	namespace     string
	bearer        string
	host          string
	resyncTimeout time.Duration
}

type controller struct {
	mx sync.Mutex

	cfg config
	cs  *kubernetes.Clientset

	stCh <-chan struct{}

	processPods  chan string
	podChangeFun map[string][]podChangeHandler
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
				fmt.Printf("could not load pod %s: %s\n", podName, err.Error())
				// TODO: How to handle this properly?
				continue
			}

			if pod.Status.Phase != coreV1.PodRunning {
				fmt.Printf("not restarting pod %s: not in running phase\n", pod.Name)
				// TODO: How to handle this properly?
				// TODO: Requeue?
				continue
			}

			fmt.Printf("restarting pod %s\n", pod.Name)

			if err := c.cs.CoreV1().Pods(c.cfg.namespace).Evict(&v1beta1.Eviction{ObjectMeta: v1.ObjectMeta{Name: pod.Name}}); err != nil {
				switch err.Error() {
				case ErrDisruptionBudget:
					// If this pod cannot be restarted then it should be queued and restarted once the one of the replicas
					// is started
					fmt.Printf("cannot restart %s: disrupting budget\n", podName)
					func(p string) {
						c.onPodChange(podName, func() {
							fmt.Printf("called: %s\n", p)
							c.processPods <- p
						})
					}(pod.Name)
				default:
					fmt.Printf("error restarting pod: %s\n", err.Error())
				}
			}
		default:
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

			if oldSecret.ResourceVersion == newSecret.ResourceVersion {
				// Secrets are the same, just continue
				return
			}

			pods, err := c.cs.CoreV1().Pods(c.cfg.namespace).List(v1.ListOptions{})
			if err != nil {
				fmt.Printf("error fetching pods: " + err.Error())
				return
			}

			var ps []string

			// Find the pods using this secret
		podsItr:
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
							fmt.Printf("secret %s changed after %s\n", s.Name, p.Name)
							ps = append(ps, p.Name)
							continue podsItr
						}
					}
				}
			}

			ps = unique(ps)

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
				fmt.Printf("pod %s changed state from pending to running\n", newPod.Name)

				podName := formatName(newPod.Name)

				c.mx.Lock()
				fs, ok := c.podChangeFun[podName]
				c.mx.Unlock()

				if ok {
					c.mx.Lock()
					delete(c.podChangeFun, podName)
					c.mx.Unlock()

					for _, f := range fs {
						f()
					}
				}
			}
		},
	})

	ctrl.Run(c.stCh)
}

func (c *controller) onPodChange(pod string, f podChangeHandler) {
	idx := strings.LastIndex(pod, "-")
	if idx > -1 {
		pod = pod[:idx]
	}

	c.mx.Lock()
	c.podChangeFun[pod] = append(c.podChangeFun[pod], f)
	c.mx.Unlock()
}

func newController(cfg config) *controller {
	//TODO: Should the clientset be initialized here?
	return &controller{
		cfg:          cfg,
		processPods:  make(chan string),
		podChangeFun: make(map[string][]podChangeHandler),
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

func formatName(name string) string {
	idx := strings.LastIndex(name, "-")
	if idx > -1 {
		name = name[:idx]
	}
	return name
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
