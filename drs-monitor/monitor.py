import subprocess
import queue
import time
import threading
import socket
from prettytable import PrettyTable

avgtime = 2

last_in = 0.0
last_out = 0.0
last_time = 0.0

cpu_q = queue.Queue(maxsize = 10)
mem_q = queue.Queue(maxsize = 10)
net_q = queue.Queue(maxsize = 10)
io_q = queue.Queue(maxsize = 10)

class MonitorThread(threading.Thread):
    def __init__(self, name, func):
        threading.Thread.__init__(self)
        self.name = name
        self.func = func

    def run(self):
        print('[INFO] Start the {} thread...'.format(self.name))
        self.func()
        print('[INFO] Exit the {} thread!'.format(self.name))

def getCpuUsage():
    command = "top -b -n 6 -d 0.1 | awk -F ',' '/%Cpu/{print $4}'"
    state, data = subprocess.getstatusoutput(command)
    if state != 0:
        print('[ERROR] Command {} failed!'.format(command))
    else:
        cpu_free = list(map(float, data.replace(' ', '').replace("id", '').split('\n')[1:]))
        cpu_usage = float('%.2f' % (100 - sum(cpu_free) / len(cpu_free)))
        return cpu_usage

def cpuQueue():
    global cpu_q
    while True:
        if cpu_q.full():
            cpu_q.get()
        cpu_q.put(getCpuUsage())
        time.sleep(0.5)

def getCpuState(num):
    global cpu_q
    state_list = list(cpu_q.queue)[-1 * num:]
    # print('[INFO] The {} latest cpu_usages are {}'.format(num, state_list))
    cpu_state = float('%.2f' % (sum(state_list) / len(state_list)))
    # print('[INFO] CPU usage: {}%'.format(cpu_state))
    return cpu_state

def getMemUsage():
    command = "head -n 3 /proc/meminfo"
    state, data = subprocess.getstatusoutput(command)
    if state != 0:
        print('[ERROR] Command {} failed!'.format(command))
    else:
        data = data.replace(' ', '').split('\n')
        mem_total = int(data[0].split(':')[1][:-2])
        mem_available = int(data[2].split(':')[1][:-2])
        mem_usage = float('%.2f' % ((mem_total - mem_available) * 100 / mem_total))
        return mem_usage

def memQueue():
    global mem_q
    while True:
        if mem_q.full():
            mem_q.get()
        mem_q.put(getMemUsage())
        time.sleep(0.5)

def getMemState(num):
    global mem_q
    state_list = list(mem_q.queue)[-1 * num:]
    # print('[INFO] The {} latest mem_usages are {}'.format(num, state_list))
    mem_state = float('%.2f' % (sum(state_list) / len(state_list)))
    # print('[INFO] Memory usage: {}%'.format(mem_state))
    return mem_state

def getNetUsage():
    command = "awk '/ens33/{print $2,$10}' /proc/net/dev"
    now = time.time()
    state, net = subprocess.getstatusoutput(command)
    now_in, now_out = [int(item) for item in net.split(' ')]
    return now_in, now_out, now

def netQueue():
    global net_q
    while True:
        if net_q.full():
            net_q.get()
        n_in, n_out, n = getNetUsage()
        recv = float('%.2f' % ((n_in - last_in) / (n - last_time) / 1024))
        tran = float('%.2f' % ((n_out - last_out) / (n - last_time) / 1024))
        net_q.put([recv, tran])
        time.sleep(0.5)

def getNetState(num):
    global net_q
    state_list = list(net_q.queue)[-1 * num:]
    in_states = []
    out_states = []
    for s in state_list:
        in_states.append(s[0])
        out_states.append(s[1])
    in_state = float('%.2f' % (sum(in_states) / len(in_states)))
    out_state = float('%.2f' % (sum(out_states) / len(out_states)))
    return in_state, out_state

def getIORate():
    command = "sudo iotop -k -P -o -n 4 -b | awk '/Total/{print $4,$5,$10,$11}'"
    state, data = subprocess.getstatusoutput(command)
    if state != 0:
        print('[ERROR] Command {} failed!'.format(command))
    else:
        data = data.split('\n')[1:]
        read = []
        write = []
        for d in data:
            tmp = d.split(' ')
            if tmp[1] == 'M/s':
                read.append(float(tmp[0]) * 1024)
            else:
                read.append(float(tmp[0]))
            if tmp[3] == 'M/s':
                write.append(float(tmp[2]) * 1024)
            else:
                write.append(float(tmp[2]))
        read_rate = float('%.2f' % (sum(read) / len(read)))
        write_rate = float('%.2f' % (sum(write) / len(write)))
        return read_rate, write_rate

def ioQueue():
    global io_q
    while True:
        if io_q.full():
            io_q.get()
        r, w = getIORate()
        io_q.put([r, w])
        time.sleep(0.5)

def getIOState(num):
    global io_q
    state_list = list(io_q.queue)[-1 * num:]
    read_states = []
    write_states = []
    for s in state_list:
        read_states.append(s[0])
        write_states.append(s[1])
    read_state = float('%.2f' % (sum(read_states) / len(read_states)))
    write_state = float('%.2f' % (sum(write_states) / len(write_states)))
    # print('[INFO] Read rate: {}K/s'.format(read_state))
    # print('[INFO] Write rate: {}K/s'.format(write_state))
    return read_state, write_state

def getState():
    cpu_state = getCpuState(avgtime)
    mem_state = getMemState(avgtime)
    in_state, out_state = getNetState(avgtime)
    read_state, write_state = getIOState(avgtime)
    table = PrettyTable(["CPU Usage", "Memory Usage", "Traffic In", "Tranffic Out", "Read Rate", "Write Rate"])
    table.add_row(["{}%".format(cpu_state), "{}%".format(mem_state), "{}KB/s".format(in_state), "{}KB/s".format(out_state), "{}KB/s".format(read_state), "{}KB/s".format(write_state)])
    print(table)
    return cpu_state, mem_state, in_state, out_state, read_state, write_state

def monitorInit():
    print('[INFO] Waiting for monitor initialization...')
    global cpu_q
    while(cpu_q.qsize() < avgtime or mem_q.qsize() < avgtime or net_q.qsize() < avgtime or io_q.qsize() < avgtime):
        time.sleep(1)
    print('[INFO] Monitor initialize over!')
    return

if __name__ == "__main__":

    last_in, last_out, last_time = getNetUsage()

    cpu_thread = MonitorThread("CPU-Monitor", cpuQueue)
    mem_thread = MonitorThread("Memory-Monitor", memQueue)
    net_thread = MonitorThread("Network-Monitor", netQueue)
    io_thread = MonitorThread("IO-Monitor", ioQueue)

    cpu_thread.start()
    mem_thread.start()
    net_thread.start()
    io_thread.start()

    monitorInit()

    host = "192.168.1.145"
    port = 9000
    connect = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    connect.bind((host, port))
    connect.listen(128)
    print("[INFO] Start listening at port {}...".format(port))
    sock, addr = connect.accept()
    print("[INFO] Got connection from {}".format(sock.getpeername()))
    while True:
        data = sock.recv(1024)
        if not data:
            break
        else:
            print("[INFO] Recieve message {}".format(data))
            state = getState()
            sock.send(str(state).encode("utf-8"))