#!/bin/bash

kubectl label nodes node1 node-role.kubernetes.io/worker=
kubectl label nodes node2 node-role.kubernetes.io/worker=
kubectl label nodes node3 node-role.kubernetes.io/worker=
kubectl label nodes node4 node-role.kubernetes.io/worker=
