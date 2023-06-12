from gym.envs.registration import register

register(
    id = "k8s-v0",
    entry_point = "myenv.K8sEnv:K8sEnv"
)