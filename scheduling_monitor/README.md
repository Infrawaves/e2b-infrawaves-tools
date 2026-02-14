## E2B：查看沙箱被调度到了哪个物理节点上

1. 在每台裸金属节点上启动一个用来监控 firecracker 的 agent (后台模式)：

```bash
nohup ./firecracker_agent.sh -i 5 -d /mnt/nfs/monitor_logs/ &

# agent 每隔 5 秒，会将当前节点上的沙箱信息写到目录 /mnt/nfs/monitor_logs/ 下，文件命名为 ip.txt, 其中，ip 表示当前物理节点的 ip。
```

2. 当需要查看整个集群的沙箱调度结果时：

```bash
cd /mnt/nfs/monitor_logs/
python3 found_sandbox_node.py *.txt

# 或者查看更详细的信息：
python3 found_sandbox_node.py *.txt --verbose
```