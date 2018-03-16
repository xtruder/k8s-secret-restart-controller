# k8s-secret-restart-controller

Kubernetes controller that automatically restarts pods on secret changes

## Testing locally with telepresence

```bash
kubectl apply -f deploy/test/*
telepresence -s secret-restart-controller
proot -b $TELEPRESENCE_ROOT/var/run/secrets/:/var/run/secrets bash
```

**Building and running**

```
go build .
./k8s-secret-restart-controller
```

**Creating example deployment with secret**

```
kubectl apply -f example/*
```

Now edit a secret and watch pods getting restarted

```
kubectl edit secret restart-on-secret-change
kubectl get pods -w
```
