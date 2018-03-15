package controller

import (
	"log"
	"time"

	coreV1 "k8s.io/api/core/v1"
	"k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

var errDisruptionBudget = "Cannot evict pod as it would violate the pod's disruption budget."

// Config for controller
type Config struct {
	Namespace    string
	ResyncPeriod time.Duration
}

// Controller is a k8s-secret-restart controller
type Controller struct {
	cfg *Config
	cs  *kubernetes.Clientset

	stCh <-chan struct{}

	processPods chan coreV1.Pod
}

// Run starts k8s-secret-restart controller main loop
func (c *Controller) Run(stChan <-chan struct{}) {
	c.stCh = stChan

	go c.restartPods()
	go c.monitorSecrets()

	go func() {
		select {
		case <-c.stCh:
			close(c.processPods)
		}
	}()
}

func (c *Controller) restartPods() {
	for {
		select {
		case pod := <-c.processPods:
			log.Printf("[INFO] restarting pod %s\n", pod.Name)

			if err := c.cs.CoreV1().Pods(c.cfg.Namespace).Evict(&v1beta1.Eviction{ObjectMeta: v1.ObjectMeta{Name: pod.Name}}); err != nil {
				switch err.Error() {
				case errDisruptionBudget:
					// If this pod cannot be restarted then it should be queued and restarted after a while
					log.Printf("[INFO] cannot restart %s: disrupting budget", pod.Name)
					time.AfterFunc(5*time.Second, func() {
						c.processPods <- pod
					})
				default:
					log.Printf("[ERROR] error restarting pod: %s", err.Error())
				}
			}
		default:
		}
	}
}

func (c *Controller) monitorSecrets() {
	lw := cache.NewListWatchFromClient(c.cs.CoreV1().RESTClient(), "secrets", c.cfg.Namespace, fields.Everything())

	_, ctrl := cache.NewInformer(lw, &coreV1.Secret{}, c.cfg.ResyncPeriod, cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			// Check if the value of the secret changed
			oldSecret := oldObj.(*coreV1.Secret)
			newSecret := newObj.(*coreV1.Secret)

			if oldSecret.ResourceVersion == newSecret.ResourceVersion {
				// Secrets are the same, just continue
				return
			}

			pods, err := c.cs.CoreV1().Pods(c.cfg.Namespace).List(v1.ListOptions{})
			if err != nil {
				log.Printf("[ERROR] error fetching pods: %s", err.Error())
				return
			}

			// Find the pods using this secret
			for _, pod := range pods.Items {

				// if pod has annotation then restart
				if val, ok := pod.Annotations["secret-restart-controller/restart"]; !ok || val != "true" {
					continue
				}

				// If at least one of the containers is using the secret then restart the whole pod
				for _, container := range pod.Spec.Containers {
					for _, env := range container.Env {
						if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil && env.ValueFrom.SecretKeyRef.Name == newSecret.Name {
							// A container in this pod is using a secret that just got updated
							// We can restart the pod

							// If the pod is running then restart it. If the pod is pending then store the event and process
							// it once the state of the pod changes to running, then check if the secrets match. If they
							// don't then restart it.
							log.Printf("[INFO] pod %s:%s has secret %s", pod.Name, container.Name, env.Name)

							c.processPods <- pod
						}
					}
				}
			}
		},
	})

	ctrl.Run(c.stCh)
}

// New method creates a new k8s-secret-restart controller
func New(cfg *Config, kconfig *rest.Config) (*Controller, error) {
	cs, err := kubernetes.NewForConfig(kconfig)

	if err != nil {
		return nil, err
	}

	return &Controller{
		cfg:         cfg,
		cs:          cs,
		processPods: make(chan coreV1.Pod),
	}, nil
}
