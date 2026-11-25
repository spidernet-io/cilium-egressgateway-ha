# 设计细节

本文是[设计文档](../design.md)的参考资料，记录 CRD、探测、agent、webhook、部署的实现细节，以及方案选择的理由。Cilium 相关的行为见 [Cilium Egress Gateway 行为分析](cilium-behavior.md)。

## 1. EgressGatewayHA CRD

### 1.1 完整字段

集群级资源，API group `egressgateway-ha.spidernet.io`：

```yaml
apiVersion: egressgateway-ha.spidernet.io/v1alpha1
kind: EgressGatewayHA
metadata:
  name: default                     # 全局只有一个实例，名字固定为 default
spec:
  egressNodeSelector:               # 出口节点，即可以成为主网关的节点
    matchLabels:
      egressgateway-ha.spidernet.io/egress-node: "true"
  interface: eth0                   # 可选。agent 把出口 IP 配到这块网卡上，不填时自动选择。不会写入 policy
  egressIPs:                        # 允许使用的出口 IP，支持单个 IP 和地址范围
  - 192.168.10.100-192.168.10.110
  - 192.168.10.120
  defaultEgressIP: 192.168.10.100   # 可选。policy 未填写 egressIP 时使用，不填时取 egressIPs 中的第一个
  activeNode: ""                    # 当前主网关。默认为空，由 controller 选取后写入；运维修改即手动切换
status:
  activeNode:                       # 主网关上出口 IP 的实际状态（主网关的 agent 写）
    name: node1
    egressIPs: [192.168.10.100]     # 已经配置到网卡上的出口 IP
    ready: true                     # 所有出口 IP 都已经配置
  egressNodes:                      # 各出口节点的 TCP 探测结果（controller 写）
  - name: node1
    reachable: true
    lastTransitionTime: "2026-11-25T08:15:40Z"   # reachable 最近一次变化的时间
  - name: node2
    reachable: false
    lastTransitionTime: "2026-11-25T09:30:12Z"
    message: "probe to 192.168.10.12:10250 failed: i/o timeout"   # 最近一次探测失败的原因，可达时为空
  conditions: [...]                 # 见 1.3
```

- CRD schema 中用 CEL 规则（`self.metadata.name == 'default'`）限制只能创建一个实例。
- `kubectl get egressgatewayha` 通过 printer columns 显示 `ACTIVE`（`spec.activeNode`）、`REACHABLE`、`READY`、`AGE`。
- status 中不单独记录"已经完成切换的节点"。切换是否完成，以 `egressgateway-ha.spidernet.io/active-node` label 实际所在的节点为准，避免维护一份需要和 label 保持一致的状态。
- 进行中的切换记录在 annotation `egressgateway-ha.spidernet.io/switch` 中（JSON：`from`、`to`、`reason`、`message`），和 `spec.activeNode` 在同一次 patch 中写入，切换完成后删除。controller 重启后据此继续切换，并在完成时写出 `ActiveNodeElected` 的 reason。`spec.activeNode` 变化了、但 annotation 中的 `to` 不等于它时，说明是运维修改的，按手动切换处理。

### 1.2 spec 的创建方式

- Helm values 中的 `global.egressNodeSelector`，以及 `egressGatewayHA` 部分（`interface`、`egressIPs`、`defaultEgressIP`），渲染成 controller 的配置。
- controller 启动时，如果 `EgressGatewayHA default` 不存在，就按这份配置创建它，`spec.activeNode` 留空。
- **CR 已经存在时，controller 不会用 Helm 配置覆盖它。** 之后以 CR 为准，运维直接修改 CR 即可调整配置。`helm upgrade` 修改这些 value 不会生效。
- agent DaemonSet 的 nodeAffinity 由同一个 Helm value `global.egressNodeSelector` 渲染。如果运维之后只修改了 CR 中的 `egressNodeSelector`，两者会不一致，见 5.2。

### 1.3 conditions

按一次完整的切换过程设计。每个 condition 都有标准的 `reason` 和 `message`，message 写明具体的节点、IP 和错误。

| type | 写入方 | 含义 | reason | message 示例 |
|---|---|---|---|---|
| `ActiveNodeElected` | controller | 已经选出主网关，并且 label 已经在这个节点上 | `Initial`（初次部署）、`Failover`（故障切换）、`Manual`（运维修改）、`EgressNodeRemoved`（原主网关不再是出口节点） | `node2 became active node, failover from node1: probe failed` |
| `ActiveNodeReachable` | controller | 主网关的 TCP 探测结果 | `ProbeSucceeded`、`ProbeFailed`；还没有探测结果时为 `Probing`，没有主网关时为 `NoActiveNode`（status 都为 Unknown） | `3/3 probes to 172.18.0.3:10250 failed: i/o timeout` |
| `ActiveNodeReady` | controller | 主网关 Node 的 Ready 状态 | 沿用 Node Ready condition 的 reason，例如 `KubeletReady`、`NodeStatusUnknown`；Node 不存在时为 `NodeNotFound`；还没有主网关时为 `NoActiveNode`（status 为 Unknown） | `node1 NotReady since 08:00:01: Kubelet stopped posting node status` |
| `Switching` | controller | `egressgateway-ha.spidernet.io/active-node` label 还没有打在 `spec.activeNode` 节点上，正在切换 | `WaitingForGateway`、`EgressIPConflict`、`NoAvailableEgressNode`、`TargetUnavailable`、`Completed` | `switching node1 -> node2: waiting for egress IPs on node2` |
| `GatewayReady` | 主网关的 agent | 所有出口 IP 都已经配置到主网关的网卡上 | `EgressIPsConfigured`、`NoManagedPolicy`（没有被管理的 policy 使用出口 IP，status 为 True）、`EgressIPConflict`、`InterfaceNotFound`、`ConfigureFailed` | `192.168.10.100 is answered by aa:bb:cc:dd:ee:ff, not added` |
| `Ready` | controller | 汇总状态，用于 `kubectl wait --for=condition=Ready` 和 ArgoCD 健康检查 | `AsExpected`，或者第一个异常 condition 的 type | `ActiveNodeReachable: 3/3 probes to 172.18.0.3:10250 failed` |

- `Ready=True` 的条件：`ActiveNodeElected`、`ActiveNodeReachable`、`ActiveNodeReady`、`GatewayReady` 都为 True，并且 `Switching=False`。不满足时，reason 取第一个异常的 condition，检查顺序为 `ActiveNodeElected`、`Switching`、`ActiveNodeReachable`、`ActiveNodeReady`、`GatewayReady`。切换期间 reason 是 `Switching`，可以直接看到切换停在哪一步。
- `GatewayReady` 只在 `status.activeNode.name` 等于 `spec.activeNode` 时有效，否则汇总时视为还没有上报。
- `ActiveNodeReachable` 只在连续失败达到 `failureThreshold` 时才变为 False。尚未达到阈值时保持 True，在 message 中写明失败次数，例如 `1/3 probes failed`。
- `Switching` 的 reason：
  - `WaitingForGateway`：已经写入 `spec.activeNode`，等待新节点配好出口 IP。
  - `EgressIPConflict`：新节点的重复地址检测发现冲突。
  - `NoAvailableEgressNode`：没有可用的出口节点。
  - `TargetUnavailable`：运维指定的目标节点不可用，`spec.activeNode` 已经被恢复。
  - `Completed`：切换完成，`Switching=False`，message 写明从哪个节点切到哪个节点。

### 1.4 status 的写入方

- controller 只知道出口 IP **应该**配在哪个节点，不知道这个节点的网卡上**是否已经配好**。两阶段切换需要在出口 IP 配好之后才修改 label，所以 `status.activeNode` 和 `GatewayReady` condition 由主网关上的 agent 写入，其余字段由 controller 写入。
- 只有 `spec.activeNode` 节点上的 agent 写 status，备节点的 agent 不写，所以同一时刻只有一个 agent 写。
- 两方都用 Server-Side Apply 更新 status，使用不同的 field manager，各自只管理自己的字段。conditions 声明为 `listType=map`、`listMapKey=type`，所以按 condition 类型分属不同的 field manager，不会互相覆盖。切换主网关时，新主网关的 agent 用 force apply 接管 `status.activeNode` 和 `GatewayReady`。
- 切换过程中，新节点的 agent 写入之前，`status.activeNode` 仍然是旧主网关留下的内容。所以 controller 只有在 `status.activeNode.name` 等于 `spec.activeNode` 时，才认为它有效。
- `egressNodes` 只记录 TCP 探测结果，并且只在 `reachable` 变化（或失败原因变化）时更新，不记录每次探测的时间，避免每个探测周期都写一次 status。message 不带连续失败次数，原因同上；次数见 `ActiveNodeReachable` condition。选择新主网关时用到的 Node Ready、cilium-agent 和 agent Pod 的就绪状态，controller 直接从 Node 和 Pod 对象读取，不写进 status。
- controller 每次 reconcile 都直接从 API Server 读取 `EgressGatewayHA`，以及带 `egressgateway-ha.spidernet.io/active-node` label 的 Node 和 CiliumNode（按 label selector 过滤，对象很少），不使用 informer 缓存。刚写完 status 或 label 后，缓存可能还是旧的；用旧数据计算 conditions 并写回，会覆盖刚写入的结果，或者把 controller 自己移动的 label 误判为手动切换。其他信息（Node Ready、出口节点 label、Pod 就绪状态）仍然从缓存读取，短暂过期只会推迟判断。

## 2. 节点故障探测

### 2.1 TCP 探测

controller 向节点的 InternalIP 发起 TCP 连接，目标端口为 `egressNodeProbe.port`（默认 10250，kubelet 端口），超时 `egressNodeProbe.timeout`（默认 2s）。controller 对所有出口节点都执行探测，结果写入 `status.egressNodes[].reachable`。

TCP 探测可以用 `controller.egressNodeProbe.enabled` 关闭，影响见[设计文档](../design.md) 4.1 节。关闭后 controller 不再探测，`status.egressNodes` 和 `ActiveNodeReachable` condition 也会从 status 中删除。

探测是**串行**的：上一次探测结束（成功、被拒绝或超时）后，等待 `egressNodeProbe.interval`（默认 5s）再发起下一次。所以同一个节点不会同时有两个探测，timeout 也可以大于 interval。

节点宕机时的检测时间：

```
检测时间 ≈ failureThreshold × timeout + (failureThreshold − 1) × interval + [0, interval]
         = 3 × 2s + 2 × 5s + [0, 5s]
         ≈ 16～21s
```

- 最后一项是宕机时刻到下一次探测开始的等待时间。
- interval 默认 5s，是为了减少探测频率，并让连续 3 次失败覆盖约 15s 以上的时间窗口，避免短时间的网络抖动触发切换。
- 调小 `timeout` 或 `interval` 可以缩短检测时间，但网络抖动时更容易误判。`timeout` 不建议小于 1s，否则内核来不及重传 SYN。

| 结果 | 判定 | 含义 |
|---|---|---|
| 连接成功 | 存活 | 节点内核和网络正常 |
| 连接被拒绝（`ECONNREFUSED`，对端回 RST） | 存活 | 节点内核收到了 SYN 并回了 RST，说明节点没有宕机，往返网络都是通的，只是 kubelet 没有监听，例如 kubelet 停止或重启 |
| 连接超时 | 失败 | 节点宕机，或者网络不可达 |
| 其他错误，例如 `EHOSTUNREACH`、`ENETUNREACH` | 失败 | 这些错误由 controller 所在节点的内核或中间路由器产生，不是目标节点的回应。例如节点宕机后，邻居表项失效时，本机内核会返回 `EHOSTUNREACH` |

实现时必须按错误码区分，**只有 `ECONNREFUSED` 视为存活**，不能把所有"立即返回的错误"都当作存活。

探测只有 controller 一个观察点。为了避免 controller 所在节点自己的网络问题被误判为主网关故障，因 TCP 探测失败而切换前，还要确认至少有一个备选节点 TCP 探测成功。如果所有出口节点都探测失败，判定为 controller 自己的网络问题，不切换，只告警：`Switching` 的 reason 为 `NoAvailableEgressNode`，message 写明其他出口节点也都不可达，并记录 Warning Event。

实现细节：

- 探测目标是所有出口节点，以及当前主网关（即使它已经不是出口节点），地址是 Node 的第一个 IPv4 InternalIP。没有 IPv4 InternalIP 的节点不能被选为主网关。
- 只有 Lease 选出的工作实例会探测。探测结果变化时（第一次有结果、成功和失败互相转换、连续失败次数变化），立即触发一次 reconcile。
- 选择新主网关时，要求节点**最近一次**探测成功；判定主网关故障时，要求**连续失败达到** `failureThreshold`。所以只失败过 1、2 次的节点不会被选为新主网关，但如果它是主网关，也不会因此被切走。
- `status.egressNodes` 中，还没有探测结果的节点 `reachable` 为 false。
- controller 重启或者工作实例切换后，在第一次探测完成之前，保留原来的 `ActiveNodeReachable`，避免 `Ready` 短暂变为 False。

### 2.2 为什么用 TCP 连接

不用 ping 或 HTTP healthz，原因如下：

- **只反映节点和网络。** HTTP healthz 反映的是 kubelet 进程是否健康。kubelet 的状态已经由 Node Ready 反映，TCP 探测只需要判断节点内核和网络是否正常。
- **有重传，不容易误判。** 单次连接超时内，内核会自动重传 SYN。Linux 的 SYN 初始重传间隔是 1s，2s 超时内会发送 2 个 SYN。ping 没有重传，而且 ICMP 经常被交换机和主机限速，内核默认也有 `icmp_ratelimit`，网络拥塞时容易误判。
- **不需要额外权限。** 只建立 TCP 连接，不发送 HTTP 请求，不需要 kubelet 的认证（`nodes/healthz` 或 `nodes/proxy` 权限），也不需要 `NET_RAW` capability。kubelet 的 `10248/healthz` 默认只监听 127.0.0.1，从其他节点访问不到。

### 2.3 kind 实测

从 control-plane 探测 worker3：

| 场景 | TCP 连接 10250 | HTTPS `10250/healthz` |
|---|---|---|
| 正常 | 成功 | 匿名访问返回 401；带 token 时返回 200（需要 `nodes/healthz` 权限） |
| 停止 kubelet | 0.01s 内被拒绝 → **存活** | 失败 |
| `docker pause` 节点（进程冻结，内核和 eBPF 仍在转发） | 成功 → **存活** | 超时 |
| `docker stop` 节点 | 连续 4 次都等满 5s 超时 → **失败** | 超时 |

### 2.4 Node Ready

- Node 的 `Ready` condition 为 `False` 或 `Unknown` 时，也判定主网关故障。
- kubelet 停止上报后，要等 kube-controller-manager 的 `node-monitor-grace-period`（默认约 40～50s）才变为 `Unknown`。
- NotReady 时数据面不一定有问题，例如只停 kubelet 时，节点仍然能正常转发，这时切换会断开已有连接。但 NotReady 表示 Kubernetes 已经认为这个节点不健康，后续可能被驱逐或下线，所以也作为切换条件。

### 2.5 不触发切换的信号

| 信号 | 原因 |
|---|---|
| `NetworkUnavailable` 条件 | Cilium 只在 agent 启动时设置一次，不能反映运行时故障（见 [Cilium 行为分析](cilium-behavior.md) 第 6 节） |
| 主网关上的 agent Pod 未就绪 | agent 升级或重启时不删除出口 IP，转发不受影响 |
| 主网关上的 cilium-agent 未就绪 | cilium-agent 重启时 BPF map 保持不变，转发不受影响 |

这些信号只用来记录 Event 和 metrics。agent Pod 和 cilium-agent 的就绪状态，还会在选择新主网关时用来过滤目标节点。

## 3. agent

### 3.1 启动

1. 读取 `EgressGatewayHA` 和所有被管理的 policy。
2. 如果本节点是 `spec.activeNode`，确保出口 IP 存在（见 3.2），**不先删除**。升级前的旧 Pod 留下的出口 IP 会被直接沿用，流量不受影响。
3. 如果本节点不是 `spec.activeNode`，删除本机所有被管理的出口 IP。
4. 如果启动时无法访问 API Server，**保持网卡现状不变**，等能访问后再执行上面的步骤。
5. 如果 `EgressGatewayHA` 不存在，也保持网卡现状不变：没有可以执行的决定，误删 CR 也不影响出口流量。

出口 IP 添加时带地址 label `<网卡名>:egw`（例如 `eth0:egw`），所以按 label 就能找到本项目管理的 IP，不依赖 API Server。Linux 要求地址 label 以网卡名开头、最长 15 字节，网卡名较长时后缀会被截断（例如 `enp175s0f0np0:e`）。出口 IP 只配置在内核中，不写入节点的网络配置文件，节点重启后会自动消失。

### 3.2 稳态

agent 持续 watch `EgressGatewayHA` 和被管理的 policy，并每 `reconcileInterval`（默认 10s）reconcile 一次。无法访问 API Server 时，保持网卡现状不变。

需要配置的出口 IP 集合 = 所有被管理 policy 的 egressIP 去重后的结果。不在 `spec.egressIPs` 中的 egressIP 会被忽略。只有当所有被管理的 policy 都不再使用某个 egressIP 时，才删除这个 IP。

**本节点是 `spec.activeNode`**：

先确定出口网卡：使用 `spec.interface`；为空时自动选择，因此各节点的网卡名可以不同。

- 优先选择 IPv4 子网包含出口 IP 的网卡。依次尝试 policy 使用的出口 IP 和 `spec.egressIPs` 中每段的起始 IP，同一个 IP 匹配多块网卡时取 ifindex 最小的。/32 地址不参与匹配，因此不会匹配到已经配置的出口 IP。出口 IP 需要和这块网卡在同一个二层网段，GARP 才能生效，所以按子网选择最准确。
- 都不匹配时，选择 metric 最小的 IPv4 默认路由所在的网卡。
- 仍然找不到时，不修改已有的出口 IP，设置 `GatewayReady=False`（reason `InterfaceNotFound`），提示设置 `spec.interface`。
- 自动选择时，`GatewayReady` 的 message 带有 `(auto-detected)`，例如 `1 egress IP(s) configured on eth0 (auto-detected) of node1`。

然后：

1. 对每个还不在网卡上的出口 IP，先做**重复地址检测**（发送源地址为 0.0.0.0 的 ARP 探测，相当于 `arping -D`）：
   - 有设备回应：说明其他节点仍然持有这个 IP。不添加，设置 `GatewayReady=False`（reason `EgressIPConflict`，message 写明冲突的 IP 和回应的 MAC 地址），等待下一轮重试。
   - 没有回应：以 /32 添加到 `spec.interface`，然后发送 GARP。
   - 已经在网卡上的出口 IP 不再做重复地址检测，例如升级前留下的 IP。
2. 新增了出口 IP 时，patch 本节点 CiliumNode 的 annotation（`egressgateway-ha.spidernet.io/egress-ip-synced=<timestamp>`），触发 Cilium 重新查找 egressIP（见 [Cilium 行为分析](cilium-behavior.md) 第 3 节）。
3. 更新 status：`status.activeNode.name` 为本节点，`egressIPs` 为已配置的 IP；所有 IP 都已配置时，`ready: true`，`GatewayReady=True`。没有被管理的 policy 使用出口 IP 时，不配置任何 IP，`GatewayReady=True`，reason 为 `NoManagedPolicy`；主网关仍然照常选出和切换，policy 创建后可以立即生效。出口网卡不存在或无法自动选择时，`GatewayReady=False`，reason 为 `InterfaceNotFound`；添加 IP 等操作失败时，reason 为 `ConfigureFailed`。只在状态变化时更新。
4. 如果 `garpInterval > 0`，按间隔重发 GARP。

另外删除其他网卡上被管理的出口 IP，例如修改 `spec.interface` 后旧网卡上留下的 IP。

**本节点不是 `spec.activeNode`**：删除所有网卡上被管理的出口 IP，不写 status。

### 3.3 退出（SIGTERM）

- **不删除出口 IP**，也不修改任何 label。Pod 升级、驱逐期间，出口 IP 和 Cilium 的 BPF 配置保持不变，出口流量不受影响。新 Pod 启动后按 3.1 沿用已有 IP。
- 例外：如果本节点已经不匹配 `egressNodeSelector`（节点不再是出口节点），就删除出口 IP。DaemonSet 在节点不再匹配时会删除 agent Pod，所以这种情况会走到这里。即使本节点仍然是 `spec.activeNode` 也删除：controller 会因为 `EgressNodeRemoved` 把主网关切走，而这个节点上已经没有 agent，留下的出口 IP 会让新主网关的重复地址检测一直失败。

## 4. Webhook

controller 的两个副本都提供 webhook 服务，不依赖工作实例。

### 4.1 作用范围

- 只处理 nodeSelector 中引用了 `egressgateway-ha.spidernet.io/active-node` 的 `CiliumEgressGatewayPolicy`。`objectSelector` 只能按 label 过滤，不能看 spec，所以改用 webhook 配置中的 `matchConditions`（Kubernetes 1.30 GA）。这条 CEL 表达式由 API Server 在调用 webhook 之前执行：

  ```yaml
  matchConditions:
  - name: managed-by-egressgateway-ha
    expression: >
      has(object.spec.egressGateway) && has(object.spec.egressGateway.nodeSelector) &&
      ((has(object.spec.egressGateway.nodeSelector.matchLabels) &&
        'egressgateway-ha.spidernet.io/active-node' in object.spec.egressGateway.nodeSelector.matchLabels) ||
       (has(object.spec.egressGateway.nodeSelector.matchExpressions) &&
        object.spec.egressGateway.nodeSelector.matchExpressions.exists(e, e.key == 'egressgateway-ha.spidernet.io/active-node')))
  ```

  同时匹配 `matchExpressions`，是为了让 validating webhook 拒绝这种写法（见 4.3）。
- 处理 CREATE 和 UPDATE。UPDATE 时如果用户把 `egressgateway-ha.spidernet.io/active-node` 从 nodeSelector 中去掉，新对象不再匹配，policy 就不再由本项目管理。
- 不匹配的 policy 完全不受影响，不会被发送到 webhook，controller 故障也不会阻止它们的创建。
- 另外校验 `EgressGatewayHA`（见 4.4）。

### 4.2 Mutating

- 只补全 egressIP：`egressGateway.egressIP` 为空时，填入 `EgressGatewayHA` 的 `spec.defaultEgressIP`。
- **不补全 interface。** Cilium 不允许 policy 同时指定 `egressIP` 和 `interface`，否则所有 cilium-agent 都会忽略这个 policy（见 [Cilium 行为分析](cilium-behavior.md) 第 5 节）。出口 IP 配在哪块网卡，由 `EgressGatewayHA.spec.interface` 或 agent 自动选择决定：agent 把 IP 配到这块网卡上，Cilium 再按 egressIP 找到持有它的网卡。
- 不修改 nodeSelector，由用户自己填写。

### 4.3 Validating

拒绝以下情况：

- nodeSelector 不是只有 `matchLabels: {egressgateway-ha.spidernet.io/active-node: "true"}`，例如 value 不是 `"true"`、还有其他 label，或者使用了 `matchExpressions`。原因是：如果还有其他条件，Cilium 会要求节点同时满足，当前主网关一旦不满足，流量就会被丢弃。
- 填写了 `egressGateway.interface`（原因见 4.2）。
- 使用了多网关字段 `egressGateways`。
- egressIP 不是 IPv4。
- egressIP 不在 `EgressGatewayHA.spec.egressIPs` 中。
- egressIP 和任意节点的 InternalIP 相同。

允许以下情况：

- 多个 policy 使用同一个 egressIP。所有被管理的 policy 共用同一个主网关，同一个 IP 只需在主网关上配置一次。Cilium 在同一节点上用同一个 IP 为多个 policy 做 SNAT 没有冲突，源端口由 BPF NAT 统一分配。
- UPDATE 时修改 egressIP，只要新值通过上述校验。通过 `warnings` 提示用户：修改后已有连接会断开。

### 4.4 校验 EgressGatewayHA

- 名字必须是 `default`（CRD schema 中的 CEL 规则，不依赖 webhook）。
- `egressIPs` 中每一项都是合法的 IPv4 地址或地址范围（`起始IP-结束IP`）。`defaultEgressIP` 如果填写，必须在 `egressIPs` 中。
- `spec.activeNode` 非空并且被修改时，节点必须存在，并且匹配 `egressNodeSelector`。只在修改时检查，这样主网关的 Node 被删除后，运维仍然可以修改其他字段。
- **controller 自己的修改不经过这个 webhook**：webhook 配置中用 `matchConditions` 排除 controller 的 ServiceAccount（`request.userInfo.username != 'system:serviceaccount:<namespace>:<controller SA>'`），API Server 不会调用 webhook。原因有两个：一是故障切换时 controller 要写 `spec.activeNode`，不能依赖 webhook 是否可用；二是 controller 回滚手动切换时，原来的节点可能已经不是出口节点，不能被拒绝。controller 写入的节点由它自己的选择规则保证。
- 修改 `egressIPs` 时，如果已有被管理的 policy 使用的 egressIP 不在新的列表中，拒绝修改。

### 4.5 运行时校验

admission webhook 有超时限制（默认 10s），只能做静态校验，不能执行 ARP 探测，也不能等待出口 IP 生效。出口 IP 是否真的可用由运行时流程检查：

- agent 的重复地址检测结果（`GatewayReady` condition 的 reason 和 message）。
- controller 根据 `EgressGatewayHA` 的 conditions，在对应 policy 上记录 Event，并暴露 metrics。例如：出口 IP 冲突、出口 IP 未配置、没有可用的主网关。

### 4.6 其他

- `failurePolicy: Fail`。如果使用 `Ignore`，controller 不可用时，未填写 egressIP 的 policy 会直接进入集群，Cilium 会改用默认路由网卡的 IP，出口 IP 会悄悄出错。由于 `matchConditions` 只匹配被管理的 policy，其他 policy 不会被发送到 webhook，所以 `Fail` 不会影响它们。
- `sideEffects: None`。出口 IP 允许复用，webhook 不需要维护分配记录，没有副作用。
- TLS 证书：controller 启动时自签证书，保存到 Secret 中供两个副本共用，并 patch webhook 配置的 `caBundle`。不依赖 cert-manager。
  - CA 和服务端证书都使用 ECDSA P-256，有效期 10 年。启动时，如果 Secret 中的证书无效、不覆盖 Service 的 DNS 名称、或者 90 天内过期，就重新生成。两个副本同时启动时，以先写入 Secret 的为准。
  - 证书在进程运行期间不会变化，所以直接从内存提供给 webhook server，不写文件，也不用 certwatcher 监视文件。certwatcher 依赖 inotify，节点上 `fs.inotify.max_user_instances` 用完时（kind 等多个节点共用宿主机内核的环境中很常见），controller 会启动失败。
  - 不在运行期间轮换证书。需要更换时，删除 Secret 后重启 controller。
  - Helm chart 中的 webhook 配置不写 `caBundle`，所以 `helm upgrade` 不会清掉 controller 写入的值。
- controller 的两个副本在 webhook server 启动后才 Ready，所以 webhook Service 只会把请求转发到已经能处理请求的副本。

## 5. 部署

### 5.1 controller

- Deployment，2 副本，调度到 master 节点（容忍 control-plane taint），副本之间用 Lease 选出工作实例。
- 不需要额外的 capability：TCP 探测使用普通 socket。
- 提供 webhook Service。
- controller 和 agent 使用各自的镜像：`ghcr.io/spidernet-io/cilium-egressgateway-ha/controller` 和 `.../agent`。

### 5.2 agent

- DaemonSet，`hostNetwork: true`。调度范围通过 nodeAffinity 限制在出口节点上。
- Helm 用同一个 value `global.egressNodeSelector` 渲染 DaemonSet 的 nodeAffinity，以及 controller 创建 `EgressGatewayHA` 时使用的 `egressNodeSelector`，所以初次部署时两者一致。CR 创建后，如果运维只修改了 CR 中的 `egressNodeSelector`，两者会不一致：匹配 selector 但没有 agent Pod 的节点会被视为不可用，不会被选为主网关；有 agent Pod 但不匹配 selector 的节点不会成为出口节点。修改出口节点时，需要同时修改 CR 和 Helm value。
- capabilities：`NET_ADMIN`（增删 IP）、`NET_RAW`（发送 GARP、重复地址检测）。
- `updateStrategy` 使用 `maxSurge: 0`，避免同一节点上同时有两个 agent。
- readinessProbe：agent 连接上 API Server 并完成首次同步后才 Ready。健康检查端口使用 host 网络上的 `agent.healthPort`（默认 9790，避开 cilium-agent 占用的 9890）。controller 用 agent Pod 的 Ready 状态判断节点能否作为新主网关，不需要单独的心跳。

### 5.3 RBAC

| 资源 | controller | agent |
|---|---|---|
| `coordination.k8s.io/leases`（本命名空间） | get, create, update（选工作实例） | - |
| `egressgateway-ha.spidernet.io/egressgatewayhas` | get, list, watch, create（初次部署时创建）, update（写入 `spec.activeNode`） | get, list, watch |
| `egressgateway-ha.spidernet.io/egressgatewayhas/status` | patch（Server-Side Apply） | patch（Server-Side Apply，只写 `status.activeNode` 和 `GatewayReady` condition） |
| `nodes` | get, list, watch, patch | get（判断本节点是否仍匹配 `egressNodeSelector`） |
| `cilium.io/ciliumnodes` | get, list, watch, patch | get, patch（只更新本节点的 annotation） |
| `cilium.io/ciliumegressgatewaypolicies` | get, list, watch | get, list, watch |
| `pods`（cilium 和本项目的命名空间） | get, list, watch（判断 cilium-agent 和 agent 是否 Ready） | - |
| `secrets`（本命名空间） | get, create, update（webhook 证书） | - |
| `mutatingwebhookconfigurations` / `validatingwebhookconfigurations` | get, patch（写入 caBundle） | - |
| `events` | create | - |

### 5.4 metrics 和 Event

controller 的 metrics 在 `controller.metricsPort`（默认 8080），只有工作实例会更新；agent 的 metrics 在 host 网络的 `agent.metricsPort`（默认 9791）。chart 为两者各创建一个 headless Service，`serviceMonitor.enabled=true` 时创建 ServiceMonitor。

| metric | 类型 | 含义 |
|---|---|---|
| `egressgateway_ha_switches_total{reason}` | counter | 开始的切换次数，reason 为 `Initial`、`Failover`、`Manual`、`EgressNodeRemoved` |
| `egressgateway_ha_active_node{node}` | gauge | `spec.activeNode` 对应的节点为 1 |
| `egressgateway_ha_condition{type,status,reason}` | gauge | 每个 condition 当前的 status 和 reason 为 1，例如告警 `Switching` 的 `NoAvailableEgressNode`、`GatewayReady` 的 `EgressIPConflict` |
| `egressgateway_ha_egress_node_reachable{node}` | gauge | 出口节点最近一次 TCP 探测成功为 1 |
| `egressgateway_ha_egress_node_available{node}` | gauge | 出口节点可以成为主网关为 1 |
| `egressgateway_ha_active_node_component_ready{component}` | gauge | 主网关上的 `agent`、`cilium-agent` 就绪为 1，不触发切换 |
| `egressgateway_ha_agent_egress_ip{interface,ip}` | gauge | 本节点网卡上被管理的出口 IP |
| `egressgateway_ha_agent_dad_conflicts_total{ip}` | counter | 重复地址检测发现冲突的次数 |

Event：

- `EgressGatewayHA` 上：`SwitchStarted`、`SwitchCompleted`、`TargetUnavailable`、`NoAvailableEgressNode`；主网关上的 agent 或 cilium-agent 变为未就绪时，记录一次 `ActiveNodeAgentNotReady`、`ActiveNodeCiliumAgentNotReady`（Warning）。
- 被管理的 policy 上：`Ready` 变为 False，或者 False 的 reason 变化时，在每个被管理的 policy 上记录一次 Warning Event `EgressGatewayNotReady`，note 中写明 `Ready` 的 reason 和 message。用户查看自己的 policy 时就能看到出口 IP 冲突、没有可用的主网关等问题。

### 5.5 卸载

`helm uninstall` 只删除 controller、agent 和 webhook，不影响出口流量：

- agent 退出时节点仍然是出口节点，所以保留出口 IP；`egressgateway-ha.spidernet.io/active-node` label 也留在主网关上。
- `EgressGatewayHA default`、证书 Secret、Lease 由 controller 创建，CRD 在 chart 的 `crds/` 目录中，都不会被删除。重新安装时沿用已有的 `EgressGatewayHA`。
- 彻底清理时，先删除被管理的 policy 和 `EgressGatewayHA default`，再删除主网关上的 label 和地址 label 为 `<网卡>:egw` 的出口 IP。

### 5.6 两个 label

| label | 默认值 | 打在哪里 | 由谁维护 | 用途 |
|---|---|---|---|---|
| 出口节点 label | `egressgateway-ha.spidernet.io/egress-node=true` | 所有出口节点 | 运维 | `global.egressNodeSelector` 的默认值：agent DaemonSet 的调度范围，以及 CR 的 `spec.egressNodeSelector` |
| 主网关 label | `egressgateway-ha.spidernet.io/active-node=true` | Node 和 CiliumNode，同一时刻只有主网关 | controller | 被管理的 policy 用它作为 nodeSelector，Cilium 据此选择网关；本项目据此识别被管理的 policy |

出口节点 label 只是 selector 的默认值，可以改用节点上已有的 label，例如机房或角色。

主网关 label 通过 Helm value `global.activeNodeLabel.key` 和 `.value` 配置，以启动参数 `--active-node-label-key`、`--active-node-label-value` 传给 controller 和 agent，并用来渲染 webhook 的 `matchConditions`。约束：

- **只在安装时设置。** 安装后修改，已有 policy 的 nodeSelector 仍然引用旧 label，主网关上的旧 label 也不会被删除。需要修改时，先删除被管理的 policy、卸载，并删除节点上的旧 label，再重新安装。
- key 和 value 必须是合法的 Kubernetes label；不要使用 `kubernetes.io`、`k8s.io` 前缀，这些前缀受 kubelet 和 NodeRestriction 的限制。
- 不能被 Cilium 的 `exclude-node-label-patterns` 排除，否则 cilium-agent 不会把 Node 上的 label 同步到 CiliumNode。
- 不放进 CR 的 spec，避免运行期间被修改；controller、agent 和 webhook 配置都来自同一个 Helm value。

## 6. 方案选择

### 6.1 为什么由集中的 controller 决定主备

出口 IP 配置好以后，转发只依赖两样东西：主网关网卡上的 IP，以及 Cilium 的 BPF map，和本项目的进程、API Server 都没有关系。所以本方案按以下原则设计：

- **只按节点本身的状态切换。** controller 用 TCP 探测和 Node Ready 状态判断主网关是否故障，不用"本项目进程是否存活"来代表节点能否转发。
- **agent 只执行，不做决定。** agent 重启或升级时不删除出口 IP，已经生效的数据面保持不变。
- **只有一个地方做决定。** 决策集中在 controller 的工作实例上，出口节点之间不需要互斥，也不会出现多个节点各自判断后同时接管。

### 6.2 为什么不使用多节点匹配

多节点匹配方案的做法是：policy 的 nodeSelector 只匹配出口节点，Cilium 按节点名排序后选第一个作为主节点。主节点故障时，由某个组件删除它的 label，Cilium 就会选择下一个节点。

这个方案能实现切换，但还要回答三个问题：谁来判断主节点故障？由谁来执行切换？怎样避免两个节点同时持有出口 IP？把这些问题都解决后，就需要一个统一做决定的组件。

| 问题 | 多节点匹配 | 集中 controller（本方案） |
|---|---|---|
| 谁是主节点 | 由 Cilium 按节点名排序决定。程序要自己复现 Cilium 的选择规则，才知道出口 IP 应该配到哪个节点。这依赖 Cilium 的内部实现 | 由 controller 决定。policy 只匹配一个节点，所以出口 IP 的位置和 Cilium 的选择总是一致 |
| 故障检测 | 需要备节点自己判断。如果监听 Node Ready，默认至少要 40s | controller 用 TCP 连接探测节点，约 16～21s；Node NotReady 也触发切换 |
| 误判时是否双主 | **会。** 备节点加上出口 IP 后，旧主节点并不知道自己已被替换，还继续持有 IP，流量全部中断 | 新主节点添加 IP 前先做重复地址检测，发现冲突就不添加，也不修改 label |
| 多个备节点之间的互斥 | 需要另外加一把锁 | 只有一个 controller 实例做决定 |
| 故障节点恢复后 | 出口节点 label 已经被删除，需要运维手动加回 | 出口节点由 `egressNodeSelector` 决定，节点恢复后自动成为备节点 |
| 是否强制切回 | **会。** 出口节点 label 加回后，原主节点名字排序最靠前，Cilium 立即切回，但出口 IP 还在备节点上，流量中断 | 不会。只有 `activeNode` 节点带有 active label |

### 6.3 为什么不在每个节点上预先配置出口 IP

- 出口 IP 配在普通网卡上时，多个节点会同时响应 ARP，回包可能被送到备节点并被丢弃。实测流量全部失败。
- 出口 IP 配在 `lo` 上，并设置 `arp_ignore=1` 时，没有任何节点响应 ARP。上游需要通过静态路由或 BGP 把 IP 指向当前主节点。这只是把"迁移 IP"换成了"切换路由"，在实际网络中很难落地。

### 6.4 与 MetalLB L2 模式的对比

MetalLB L2 模式也是"一个 IP 在多个节点之间主备切换"，以下内容基于 MetalLB 源码（`speaker/layer2_controller.go`、`internal/speakerlist/speakerlist.go`，commit `6be0662`）和官方文档 `concepts/layer2.md`。

| 方面 | MetalLB L2 | 本方案 |
|---|---|---|
| 选主方式 | **无状态、分布式**。每个 speaker 都收集同一组可用节点，按 `sha256(节点名 + "#" + IP)` 排序（可以用 `preferredNodes` 加权），排第一的节点宣告这个 IP。没有 Lease，也没有中心组件 | 中心化。controller 决定主网关并写入 `spec.activeNode`，选择结果有状态、不随节点增减而变化 |
| 可用节点 | memberlist 中存活的 speaker，排除带 `NetworkUnavailable` 条件或 `node.kubernetes.io/exclude-from-external-load-balancers` label 的节点，再按 L2Advertisement 的节点选择器过滤 | 出口节点中：TCP 探测可达、Node Ready、cilium-agent Ready、agent Pod Ready |
| 故障检测 | speaker 之间通过 memberlist（SWIM gossip）互相探测，秒级 | controller 从 master 节点用 TCP 连接探测节点，秒级 |
| IP 如何生效 | **不把 IP 配到网卡上**。speaker 在用户态响应 ARP，并发送 GARP，转发由 kube-proxy 完成 | Cilium 要求 egressIP 属于节点（配在网卡上或作为 local route），所以必须把 IP 配到网卡上 |
| speaker/agent 升级 | speaker 退出时调用 memberlist `Leave`，其他 speaker 立即把它移出可用列表并重新计算，**升级会触发切换** | agent 退出时保留出口 IP，升级不切换 |
| 节点恢复 | 节点重新加入后，如果它在排序中排第一（或 `preferredNodes` 权重更高），IP 会切回去 | 不切回 |
| 脑裂 | 官方文档承认：speaker 误判节点状态时，可能有多个节点同时宣告，或者没有节点宣告 | 重复地址检测；出口 IP 配好之前不修改 label |

**借鉴的点**：

- `exclude-from-external-load-balancers` label 可以作为一个可选的排除条件。
- 支持周期性发送 GARP（MetalLB 的 `--gratuitous-arp-interval`，默认 0）。有些交换机的 FDB 老化时间比上游网关的 ARP 缓存短，需要定期刷新，对应配置中的 `garpInterval`。
- 计划内切换后，部分客户端更新 ARP 缓存可能较慢。MetalLB 的建议是旧主节点保留一段时间再下线；本方案中旧主节点必须删除出口 IP 以避免冲突，所以只能依靠 GARP。

**不借鉴的点**：

- `NetworkUnavailable` 条件：不能反映运行时故障（见 [Cilium 行为分析](cilium-behavior.md) 第 6 节）。
- 无状态哈希选主：节点增减会改变排序结果，不满足"不切回"和"升级不切换"的要求。
- 用户态 ARP 响应：Cilium 要求 egressIP 必须属于节点，不能只响应 ARP。

## 7. 故障场景实测

环境：kind（Kubernetes v1.36.1，Cilium v1.20.5，tunnel/vxlan，KPR），1 个 control-plane 和 2 个 worker 作为出口节点，client Pod 在非出口节点上。默认探测参数（2s、5s、3 次），e2e 中 agent 的 `reconcileInterval` 为 5s。测试代码在 `test/e2e/`，是按类别划分的 Ginkgo suite，每个 PR 都会在 GitHub Actions 中运行（见 README）。

出口流量的测量方法：client Pod 内运行一个循环，每 200ms 用 `curl -m 1` 访问外部目标，记录 `/proc/uptime` 和目标看到的源 IP，结果写在 Pod 内的文件中，事后读取。这样 API Server 不可用时也能测量。"最长间隔"是两次成功请求（源 IP 为出口 IP）之间的最长时间，正常值约 0.2s。

| 场景 | 操作 | 是否切换 | 出口流量 |
|---|---|---|---|
| 主网关宕机 | `docker kill` 主网关 | 是，17.1～21.1s 后写入新的 `spec.activeNode` | 18.9～22.9s 后恢复（4 次测量） |
| kubelet 停止 | `systemctl stop kubelet` | 是，48～56s 后（Node NotReady） | 切换前不受影响 |
| kubelet 短暂重启 | `systemctl restart kubelet`，观察 30s | 否 | 0 失败，最长间隔 0.22s |
| agent 滚动升级 | `kubectl rollout restart ds/egwha-agent` | 否 | 0 失败，最长间隔 0.22s |
| cilium-agent 重启 | 删除主网关上的 cilium Pod | 否 | 0 失败，最长间隔 0.21s |
| controller 主副本重启 | 删除持有 Lease 的 Pod | 否，另一个副本接管 | 0 失败，最长间隔 0.21s |
| API Server 不可用 | 移走 kube-apiserver 的静态 Pod 清单，约 65s | 否 | 0 失败，最长间隔 0.22s |
| 网络分区 | 主网关上用 iptables 丢弃发往 API Server 的包和 10250 入向连接，ARP 不受影响 | 21s 后开始切换，22s 后停在 `EgressIPConflict`，label 不动 | 分区期间 0 失败 |
| 分区恢复 | 删除 iptables 规则 | 旧主网关的 agent 约 11s 后删除出口 IP，约 16s 后切换完成 | 切换时 1 次请求失败，最长间隔 1.43s |
| 故障节点恢复 | `docker start`、`systemctl start kubelet` | 否，不切回 | 不受影响 |

补充说明：

- 网络分区场景在 e2e 中紧接着 API Server 不可用的场景运行。此时 controller 刚刚因为丢失 Lease 重启，重新选主并从头开始探测，所以 e2e 中测到的"分区后到 `EgressIPConflict`"为 46～51s。单独运行时为 22s。
- 分区期间每秒检查一次，出口 IP 始终只在一个节点上。
- 分区恢复后，旧主网关的 agent 删除出口 IP 需要约 11s，主要是 client-go 重新建立 watch 的退避时间，加上 `reconcileInterval`。
