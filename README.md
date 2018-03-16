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
