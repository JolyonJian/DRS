from flask import Flask, request
import socket
import pickle
import threading

import torch
import torch.nn as nn
import torch.nn.functional as F
import numpy as np
import gym
import myenv

pod_action = {}

app = Flask(__name__)

node1 = ("192.168.1.145", 9000)
node2 = ("192.168.1.116", 9000)
node3 = ("192.168.1.193", 9000)
node4 = ("192.168.1.199", 9000)

sock2Node1 = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
sock2Node1.connect(node1)
print("[INFO] Connect to Node1 at {}".format(sock2Node1.getpeername()))

sock2Node2 = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
sock2Node2.connect(node2)
print("[INFO] Connect to Node2 at {}".format(sock2Node2.getpeername()))

sock2Node3 = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
sock2Node3.connect(node3)
print("[INFO] Connect to Node3 at {}".format(sock2Node3.getpeername()))

sock2Node4 = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
sock2Node4.connect(node4)
print("[INFO] Connect to Node4 at {}".format(sock2Node4.getpeername()))

# hyper parameters
BATCH_SIZE = 32                                 # batch size
LR = 0.01                                       # learning rate
EPSILON = 0.9                                   # greedy policy
GAMMA = 0.9                                     # reward discount
TARGET_REPLACE_ITER = 100                       # update frequency
MEMORY_CAPACITY = 100                          # capacity of memory pool
env = gym.make('k8s-v0').unwrapped
N_ACTIONS = env.action_space.n
N_STATES = env.observation_space.shape[0]
env.setsocks(sock2Node1, sock2Node2, sock2Node3, sock2Node4)

class Net(nn.Module):
    def __init__(self):
        super(Net, self).__init__()

        self.fc1 = nn.Linear(N_STATES, 50)
        self.fc1.weight.data.normal_(0, 0.1)
        self.out = nn.Linear(50, N_ACTIONS)
        self.out.weight.data.normal_(0, 0.1)

    def forward(self, x):
        x = F.relu(self.fc1(x))
        actions_value = self.out(x)
        return actions_value

class DQN(object):
    def __init__(self):
        self.eval_net, self.target_net = Net(), Net()
        self.learn_step_counter = 0
        self.memory_counter = 0
        self.memory = np.zeros((MEMORY_CAPACITY, N_STATES * 2 + 2))
        self.optimizer = torch.optim.Adam(self.eval_net.parameters(), lr=LR)
        self.loss_func = nn.MSELoss()

    def choose_action(self, x):
        x = torch.unsqueeze(torch.FloatTensor(x), 0)
        if np.random.uniform() < EPSILON:
            actions_value = self.eval_net.forward(x)
            action = torch.max(actions_value, 1)[1].data.numpy()
            action = action[0]
        else:
            action = np.random.randint(0, N_ACTIONS)
        return action

    def store_transition(self, s, a, r, s_):
        transition = np.hstack((s, [a, r], s_))
        index = self.memory_counter % MEMORY_CAPACITY
        self.memory[index, :] = transition
        self.memory_counter += 1

    def learn(self):
        if self.learn_step_counter % TARGET_REPLACE_ITER == 0:
            self.target_net.load_state_dict(self.eval_net.state_dict())
        self.learn_step_counter += 1

        sample_index = np.random.choice(MEMORY_CAPACITY, BATCH_SIZE)
        b_memory = self.memory[sample_index, :]
        b_s = torch.FloatTensor(b_memory[:, :N_STATES])
        b_a = torch.LongTensor(b_memory[:, N_STATES:N_STATES+1].astype(int))
        b_r = torch.FloatTensor(b_memory[:, N_STATES+1:N_STATES+2])
        b_s_ = torch.FloatTensor(b_memory[:, -N_STATES:])

        q_eval = self.eval_net(b_s).gather(1, b_a)
        q_next = self.target_net(b_s_).detach()
        q_target = b_r + GAMMA * q_next.max(1)[0].view(BATCH_SIZE, 1)
        loss = self.loss_func(q_eval, q_target)
        self.optimizer.zero_grad()
        loss.backward()
        self.optimizer.step()

class StepThread(threading.Thread):
    def __init__(self, func, dqn, env, state, action):
        threading.Thread.__init__(self)
        self.func = func
        self.dqn = dqn
        self.env = env
        self.state = state
        self.action = action

    def run(self):
        print('[INFO] Start the Step thread...')
        self.func(self.dqn, self.env, self.state, self.action)
        print('[INFO] Exit the Step thread!')

def makeStep(dqn, env, state, action):
    s_, r, done, info = env.step(action)

    dqn.store_transition(state, action, r, s_)

    if env.count == 100:
        pod_action.clear()
        env.reset()

    if dqn.memory_counter > MEMORY_CAPACITY:
        t_file = open('transition.pkl', 'w')
        pickle.dump(dqn.memory, t_file)
        t_file.close()


@app.route('/choose', methods = ['POST'])
def choose():
    podname = request.form.get("podname")
    print('[INFO] Get pod: {}'.format(podname))

    if podname in pod_action:
        print('[INFO] This pod has been scheduled, action: {}'.format(pod_action[podname]))
        return pod_action[podname]

    s = env.state

    if podname.find('video') != -1:
        s_pod = [100.0, 23.0, 11.25, 2.49, 0.0, 1.54]
    elif podname.find('net') != -1:
        s_pod = [54.0, 46.2, 80.04, 71.4, 0.0, 1.58]
    elif podname.find('disk') != -1:
        s_pod = [100.0, 22.96, 12.6, 2.73, 0.0, 86.26]
    else:
        s_pod = [0.0, 0.0, 0.0, 0.0, 0.0, 0.0]

    if len(s) == 24:
        s = s + s_pod
    else:
        s[-6:] = s_pod

    action = ""
    a = dqn.choose_action(s)
    if a == 0:
        action = "node1"
    elif a == 1:
        action = "node2"
    elif a == 2:
        action = "node3"
    else:
        action = "node4"

    pod_action[podname] = action

    thread = StepThread(makeStep, dqn, env, s, a)
    thread.start()

    print('[INFO] Action for Pod {} is: {}'.format(podname, action))
    return action

if __name__ == "__main__":

    dqn = DQN()

    s = env.reset()

    print("[INFO] Environment initialize...")

    app.run(debug = False, host = "0.0.0.0", port=1234)