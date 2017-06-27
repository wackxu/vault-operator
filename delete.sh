#!/bin/sh
kubectl delete secret etcd-server-client-tls etcd-server-peer-tls operator-etcd-client-tls vault-client-tls vault-server-tls vault-etcd-client-tls
kubectl delete configmap vault
kubectl delete -f manifests/etcd-cluster.yaml -f manifests/vault.yaml
