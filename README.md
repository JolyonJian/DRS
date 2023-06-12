# DRS
A Deep Reinforcement Learning enhanced Kubernetes Scheduler for Microservice-based System

## File description
- `kubernetes/` : The source code of [Kubernetes (version 1.23.4)](https://github.com/kubernetes/kubernetes/tree/v1.23.4). The DRS scheduler is in registed in `kubernetes/pkg/scheduler/framework/plugins/dqn/dqn.go`
- `deploy/` :
    - `apps/` : the application configure file and deploy script.

        The docker images of the applications in available on DockerHub.

    | Application | Type | Description | Docker Image |
    | :----: | :----: | :----: | :----: |
    | Video Scale | CPU-intensive | Scale the video to a certain size with ffmpeg | [jolyonjian/apps:cpu-1.0](https://hub.docker.com/repository/docker/jolyonjian/apps) |
    | Transmission | Network-intensive | Transfer data of a certain size to the server | [jolyonjian/apps:net-1.0](https://hub.docker.com/repository/docker/jolyonjian/apps) |
    | Data Write | IO-intensive | Read a file on the disk and write a copy | [jolyonjian/apps:io-1.0](https://hub.docker.com/repository/docker/jolyonjian/apps) |

    - `scripts/` : Scripts for cluster creation, initialization, deletion, etc.
- `scheduler/` : DRS scheduler runs on the master node of the k8s cluster
- `monitor/` : DRS monitor runs on the worker node of the k8s cluster

## Run
1. Modify the source code of [Kubernetes (version 1.23.4)](https://github.com/kubernetes/kubernetes/tree/v1.23.4) to regist DRS scheduler and compile the project.
```
# Configure go environment

# clone the source code of Kubernetes v1.23.4
$ git clone -b v1.23.4 https://github.com/kubernetes/kubernetes.git
$ mv ./kubernetes <path of go-workspace>/

$ git clone https://github.com/JolyonJian/DRS
$ cd DRS
# Replace the source code of Kube-scheduler
$ mv ./scheduler <path of go-workspace>/kubernetes/pkg/
$ cd <path of go-workspace>/kubernetes
$ make

# After the first compilation you can only compile the kube-scheduler
$ make cmd/kube-scheduler
```
2. Initalize the Kubernetes cluster.
```
# Start a k8s cluster (on the master node)
$ cd <path of DRS>/deploy/scripts
$ ./init.sh
$ ./env.sh

# Add the worker nodes into the cluster (on each worker node)
$ kubeadm join <mater-node-ip>:6443 --token <your-token> --discovery-token-ca-cert-hash <your-cert-hash>

# Deploy the network and the second scheduler plugins
$ cd <path of DRS>/deploy/apps
$ ./apply.sh kube-flannel.yaml
$ ./apply.sh drs-scheduler.yaml
```
3. Start the DRS scheduler and DRS the monitor.
```
# Start the DRS scheduler (on the master node)
$ cd <path of DRS>/scheduler
# The node ip needs to be configured according to your environment
$ python dqn.py

# Start the DRS monitor (on each worker node)
$ cd <path of DRS>/monitor
# The node ip and port need to be configured according to your environment
$ ./monitor.sh
```

4. Deploy applications to the cluster.
```
$ cd <path of DRS>/deploy/apps
# Specify the scheduler in the configuration file
$ ./apply.sh <app.yaml>
```

## Contact
The link of our paper (Under Review): [https://www.authorea.com/doi/full/10.22541/au.167285897.72278925](https://www.authorea.com/doi/full/10.22541/au.167285897.72278925)

If you have any questions, please contact us.

Zhaolong Jian: jianzhaolong@mail.nankai.edu.cn