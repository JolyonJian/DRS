#!/bin/bash

kubectl delete -f my-scheduler.yaml
docker container prune
