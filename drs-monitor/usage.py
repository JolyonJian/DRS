import subprocess
import time
import threading

states = []
last_in = 0.0
last_out = 0.0
last_time = 0.0

class MonitorThread(threading.Thread):
    def __init__(self, id, func):
        threading.Thread.__init__(self)
        self.id = id
        self.func = func

    def run(self):
        print('[INFO] Start the {} thread...'.format(self.id))
        self.func()
        print('[INFO] Exit the {} thread!'.format(self.id))

def Cpu():
    command = "top -b -n 2 -d 0.1 | awk -F ',' '/%Cpu/{print $4}'"
    state, data = subprocess.getstatusoutput(command)
    if state != 0:
        print('[ERROR] Command {} failed!'.format(command))
    else:
        cpu_free = list(map(float, data.replace(' ', '').replace("id", '').split('\n')[1:]))
        cpu_usage = float('%.2f' % (100 - sum(cpu_free) / len(cpu_free)))
        # print(cpu_usage)
        return cpu_usage

def Memory():
    command = "head -n 3 /proc/meminfo"
    state, data = subprocess.getstatusoutput(command)
    if state != 0:
        print('[ERROR] Command {} failed!'.format(command))
    else:
        data = data.replace(' ', '').split('\n')
        mem_total = int(data[0].split(':')[1][:-2])
        mem_available = int(data[2].split(':')[1][:-2])
        mem_usage = float('%.2f' % ((mem_total - mem_available) * 100 / mem_total))
        # print(mem_usage)
        return mem_usage

def Network():
    now = time.time()
    command = "awk '/ens33/{print $2,$10}' /proc/net/dev"
    state, net = subprocess.getstatusoutput(command)
    now_in, now_out = [int(item) for item in net.split(' ')]
    return now_in, now_out, now

def Io():
    command = "sudo iotop -k -P -o -n 2 -b | awk '/Total/{print $4,$5,$10,$11}'"
    state, data = subprocess.getstatusoutput(command)
    if state != 0:
        print('[ERROR] Command {} failed!'.format(command))
    else:
        data = data.split('\n')[1].split(' ')
        if data[1] == 'M/s':
            read = float(data[0]) * 1024
        else:
            read = float(data[0])
        if data[3] == 'M/s':
            write = float(data[2]) * 1024
        else:
            write = float(data[2])
        # print(read, write)
        return read, write

def state():
    cpu = Cpu()
    mem = Memory()
    now_in, now_out, now = Network()
    read, write = Io()
    recv = float('%.2f' % ((now_in - last_in) / (now - last_time) / 1024))
    tran = float('%.2f' % ((now_out - last_out) / (now - last_time) / 1024))
    states.append([cpu, mem, recv, tran, read, write])

if __name__ == "__main__":

    last_in, last_out, last_time = Network()

    for i in range(120):
        thread = MonitorThread(i, state)
        thread.start()
        time.sleep(0.5)

    time.sleep(10)

    with open('usage.txt', 'w') as f:
        for s in states:
            f.write(str(s)[1:-1].replace(',', '') + '\n')
