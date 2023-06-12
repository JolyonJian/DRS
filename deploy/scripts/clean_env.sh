#!/bin/bash

sudo rm -rf $HOME/.kube/*
sudo rm -rf /var/lib/cni/
sudo rm -rf /var/lib/kubelet/*
sudo rm -rf /etc/kubernetes/
sudo rm -rf /etc/cni/
sudo ifconfig cni0 down
sudo ifconfig flannel.1 down
sudo ip link delete cni0
sudo ip link delete flannel.1
