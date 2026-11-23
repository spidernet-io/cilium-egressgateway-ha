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
  candidateNodeSelector:            # 候选网关节点
    matchLabels:
      node-role.example.com/egress: "true"
  interface: eth0                   # agent 把出口 IP 配到这块网卡上。不会写入 policy
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
  candidates:                       # 各候选节点的 TCP 探测结果（controller 写）
  - name: node1
    reachable: true
  - name: node2
    reachable: true
  conditions: [...]                 # 见 1.3
```

- CRD schema 中用 CEL 规则（`self.metadata.name == 'default'`）限制只能创建一个实例。
- `kubectl get egressgatewayha` 通过 printer columns 显示 `ACTIVE`（`spec.activeNode`）、`REACHABLE`、`READY`、`AGE`。
- status 中不单独记录"已经完成切换的节点"。切换是否完成，以 `egress-gw-active` label 实际所在的节点为准，避免维护一份需要和 label 保持一致的状态。

### 1.2 spec 的创建方式

- Helm values 中的 `egressGatewayHA` 部分（`candidateNodeSelector`、`interface`、`egressIPs`、`defaultEgressIP`）渲染成 controller 的配置。
- controller 启动时，如果 `EgressGatewayHA default` 不存在，就按这份配置创建它，`spec.activeNode` 留空。
- **CR 已经存在时，controller 不会用 Helm 配置覆盖它。** 之后以 CR 为准，运维直接修改 CR 即可调整配置。`helm upgrade` 修改这些 value 不会生效。
- agent DaemonSet 的 nodeAffinity 由同一个 Helm value 渲染。如果运维之后只修改了 CR 中的 `candidateNodeSelector`，两者会不一致，见 5.2。

### 1.3 conditions

按一次完整的切换过程设计。每个 condition 都有标准的 `reason` 和 `message`，message 写明具体的节点、IP 和错误。

| type | 写入方 | 含义 | reason | message 示例 |
|---|---|---|---|---|
| `ActiveNodeElected` | controller | 已经选出主网关，并且 label 已经在这个节点上 | `Initial`（初次部署）、`Failover`（故障切换）、`Manual`（运维修改）、`CandidateRemoved`（原主网关不再是候选节点） | `node2 became active node, failover from node1: probe failed` |
| `ActiveNodeReachable` | controller | 主网关的 TCP 探测结果 | `ProbeSucceeded`、`ProbeFailed` | `3/3 probes to 172.18.0.3:10250 failed: i/o timeout` |
| `ActiveNodeReady` | controller | 主网关 Node 的 Ready 状态 | 沿用 Node Ready condition 的 reason，例如 `KubeletReady`、`NodeStatusUnknown` | `node1 NotReady since 08:00:01: Kubelet stopped posting node status` |
| `Switching` | controller | `egress-gw-active` label 还没有打在 `spec.activeNode` 节点上，正在切换 | `WaitingForGateway`、`EgressIPConflict`、`NoAvailableCandidate`、`TargetUnavailable`、`Completed` | `switching node1 -> node2: waiting for egress IPs on node2` |
| `GatewayReady` | 主网关的 agent | 所有出口 IP 都已经配置到主网关的网卡上 | `EgressIPsConfigured`、`EgressIPConflict`、`InterfaceNotFound`、`ConfigureFailed` | `192.168.10.100 is answered by aa:bb:cc:dd:ee:ff, not added` |
| `Ready` | controller | 汇总状态，用于 `kubectl wait --for=condition=Ready` 和 ArgoCD 健康检查 | `AsExpected`，或者第一个异常 condition 的 type | `ActiveNodeReachable: 3/3 probes to 172.18.0.3:10250 failed` |

- `Ready=True` 的条件：`ActiveNodeElected`、`ActiveNodeReachable`、`ActiveNodeReady`、`GatewayReady` 都为 True，并且 `Switching=False`。
- `ActiveNodeReachable` 只在连续失败达到 `failureThreshold` 时才变为 False。尚未达到阈值时保持 True，在 message 中写明失败次数，例如 `1/3 probes failed`。
- `Switching` 的 reason：
  - `WaitingForGateway`：已经写入 `spec.activeNode`，等待新节点配好出口 IP。
  - `EgressIPConflict`：新节点的重复地址检测发现冲突。
  - `NoAvailableCandidate`：没有可用的候选节点。
  - `TargetUnavailable`：运维指定的目标节点不可用，`spec.activeNode` 已经被恢复。
  - `Completed`：切换完成，`Switching=False`，message 写明从哪个节点切到哪个节点。

### 1.4 status 的写入方

- controller 只知道出口 IP **应该**配在哪个节点，不知道这个节点的网卡上**是否已经配好**。两阶段切换需要在出口 IP 配好之后才修改 label，所以 `status.activeNode` 和 `GatewayReady` condition 由主网关上的 agent 写入，其余字段由 controller 写入。
- 只有 `spec.activeNode` 节点上的 agent 写 status，备节点的 agent 不写，所以同一时刻只有一个 agent 写。
- 两方都用 Server-Side Apply 更新 status，使用不同的 field manager，各自只管理自己的字段。conditions 声明为 `listType=map`、`listMapKey=type`，所以按 condition 类型分属不同的 field manager，不会互相覆盖。切换主网关时，新主网关的 agent 用 force apply 接管 `status.activeNode` 和 `GatewayReady`。
- 切换过程中，新节点的 agent 写入之前，`status.activeNode` 仍然是旧主网关留下的内容。所以 controller 只有在 `status.activeNode.name` 等于 `spec.activeNode` 时，才认为它有效。
- `candidates` 只记录 TCP 探测结果。选择新主网关时用到的 Node Ready、cilium-agent 和 agent Pod 的就绪状态，controller 直接从 Node 和 Pod 对象读取，不写进 status。

## 2. 节点故障探测

### 2.1 TCP 探测

controller 向节点的 InternalIP 发起 TCP 连接，目标端口为 `nodeProbe.port`（默认 10250，kubelet 端口），超时 `nodeProbe.timeout`（默认 2s）。controller 对所有候选节点都执行探测，结果写入 `status.candidates[].reachable`。

探测是**串行**的：上一次探测结束（成功、被拒绝或超时）后，等待 `nodeProbe.interval`（默认 5s）再发起下一次。所以同一个节点不会同时有两个探测，timeout 也可以大于 interval。

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

探测只有 controller 一个观察点。为了避免 controller 所在节点自己的网络问题被误判为主网关故障，因 TCP 探测失败而切换前，还要确认至少有一个备选节点 TCP 探测成功。如果所有候选节点都探测失败，判定为 controller 自己的网络问题，不切换，只告警。

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

1. 对每个还不在网卡上的出口 IP，先做**重复地址检测**（发送源地址为 0.0.0.0 的 ARP 探测，相当于 `arping -D`）：
   - 有设备回应：说明其他节点仍然持有这个 IP。不添加，设置 `GatewayReady=False`（reason `EgressIPConflict`，message 写明冲突的 IP 和回应的 MAC 地址），等待下一轮重试。
   - 没有回应：以 /32 添加到 `spec.interface`，然后发送 GARP。
   - 已经在网卡上的出口 IP 不再做重复地址检测，例如升级前留下的 IP。
2. 新增了出口 IP 时，patch 本节点 CiliumNode 的 annotation（`egressgateway-ha.spidernet.io/egress-ip-synced=<timestamp>`），触发 Cilium 重新查找 egressIP（见 [Cilium 行为分析](cilium-behavior.md) 第 3 节）。
3. 更新 status：`status.activeNode.name` 为本节点，`egressIPs` 为已配置的 IP；所有 IP 都已配置时，`ready: true`，`GatewayReady=True`。`spec.interface` 不存在时，`GatewayReady=False`，reason 为 `InterfaceNotFound`；添加 IP 等操作失败时，reason 为 `ConfigureFailed`。只在状态变化时更新。
4. 如果 `garpInterval > 0`，按间隔重发 GARP。

另外删除其他网卡上被管理的出口 IP，例如修改 `spec.interface` 后旧网卡上留下的 IP。

**本节点不是 `spec.activeNode`**：删除所有网卡上被管理的出口 IP，不写 status。

### 3.3 退出（SIGTERM）

- **不删除出口 IP**，也不修改任何 label。Pod 升级、驱逐期间，出口 IP 和 Cilium 的 BPF 配置保持不变，出口流量不受影响。新 Pod 启动后按 3.1 沿用已有 IP。
- 例外：如果本节点已经不匹配 `candidateNodeSelector`（节点退出候选），并且也不是 `spec.activeNode`，就删除出口 IP。DaemonSet 在节点不再匹配时会删除 agent Pod，所以这种情况会走到这里。

## 4. Webhook

controller 的两个副本都提供 webhook 服务，不依赖工作实例。

### 4.1 作用范围

- 只处理 nodeSelector 中引用了 `egress-gw-active` 的 `CiliumEgressGatewayPolicy`。`objectSelector` 只能按 label 过滤，不能看 spec，所以改用 webhook 配置中的 `matchConditions`（Kubernetes 1.30 GA）。这条 CEL 表达式由 API Server 在调用 webhook 之前执行：

  ```yaml
  matchConditions:
  - name: managed-by-egress-gw-ha
    expression: >
      has(object.spec.egressGateway) && has(object.spec.egressGateway.nodeSelector) &&
      ((has(object.spec.egressGateway.nodeSelector.matchLabels) &&
        'egress-gw-active' in object.spec.egressGateway.nodeSelector.matchLabels) ||
       (has(object.spec.egressGateway.nodeSelector.matchExpressions) &&
        object.spec.egressGateway.nodeSelector.matchExpressions.exists(e, e.key == 'egress-gw-active')))
  ```

  同时匹配 `matchExpressions`，是为了让 validating webhook 拒绝这种写法（见 4.3）。
- 处理 CREATE 和 UPDATE。UPDATE 时如果用户把 `egress-gw-active` 从 nodeSelector 中去掉，新对象不再匹配，policy 就不再由本项目管理。
- 不匹配的 policy 完全不受影响，不会被发送到 webhook，controller 故障也不会阻止它们的创建。
- 另外校验 `EgressGatewayHA`（见 4.4）。

### 4.2 Mutating

- 只补全 egressIP：`egressGateway.egressIP` 为空时，填入 `EgressGatewayHA` 的 `spec.defaultEgressIP`。
- **不补全 interface。** Cilium 不允许 policy 同时指定 `egressIP` 和 `interface`，否则所有 cilium-agent 都会忽略这个 policy（见 [Cilium 行为分析](cilium-behavior.md) 第 5 节）。出口 IP 配在哪块网卡，由 `EgressGatewayHA.spec.interface` 决定：agent 把 IP 配到这块网卡上，Cilium 再按 egressIP 找到持有它的网卡。
- 不修改 nodeSelector，由用户自己填写。

### 4.3 Validating

拒绝以下情况：

- nodeSelector 不是只有 `matchLabels: {egress-gw-active: "true"}`，例如 value 不是 `"true"`、还有其他 label，或者使用了 `matchExpressions`。原因是：如果还有其他条件，Cilium 会要求节点同时满足，当前主网关一旦不满足，流量就会被丢弃。
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
- `spec.activeNode` 非空时，节点必须存在，并且匹配 `candidateNodeSelector`。
- 修改 `egressIPs` 时，如果已有被管理的 policy 使用的 egressIP 不在新的列表中，拒绝修改。

### 4.5 运行时校验

admission webhook 有超时限制（默认 10s），只能做静态校验，不能执行 ARP 探测，也不能等待出口 IP 生效。出口 IP 是否真的可用由运行时流程检查：

- agent 的重复地址检测结果（`GatewayReady` condition 的 reason 和 message）。
- controller 根据 `EgressGatewayHA` 的 conditions，在对应 policy 上记录 Event，并暴露 metrics。例如：出口 IP 冲突、出口 IP 未配置、没有可用的主网关。

### 4.6 其他

- `failurePolicy: Fail`。如果使用 `Ignore`，controller 不可用时，未填写 egressIP 的 policy 会直接进入集群，Cilium 会改用默认路由网卡的 IP，出口 IP 会悄悄出错。由于 `matchConditions` 只匹配被管理的 policy，其他 policy 不会被发送到 webhook，所以 `Fail` 不会影响它们。
- `sideEffects: None`。出口 IP 允许复用，webhook 不需要维护分配记录，没有副作用。
- TLS 证书：controller 启动时自签证书，保存到 Secret 中供两个副本共用，并 patch webhook 配置的 `caBundle`。不依赖 cert-manager。

## 5. 部署

### 5.1 controller

- Deployment，2 副本，调度到 master 节点（容忍 control-plane taint），副本之间用 Lease 选出工作实例。
- 不需要额外的 capability：TCP 探测使用普通 socket。
- 提供 webhook Service。

### 5.2 agent

- DaemonSet，`hostNetwork: true`。调度范围通过 nodeAffinity 限制在候选节点上。
- Helm 用同一个 value 渲染 DaemonSet 的 nodeAffinity，以及 controller 创建 `EgressGatewayHA` 时使用的 `candidateNodeSelector`，所以初次部署时两者一致。CR 创建后，如果运维只修改了 CR 中的 `candidateNodeSelector`，两者会不一致：匹配 selector 但没有 agent Pod 的节点会被视为不可用，不会被选为主网关；有 agent Pod 但不匹配 selector 的节点不会成为候选节点。修改候选节点时，需要同时修改 CR 和 Helm value。
- capabilities：`NET_ADMIN`（增删 IP）、`NET_RAW`（发送 GARP、重复地址检测）。
- `updateStrategy` 使用 `maxSurge: 0`，避免同一节点上同时有两个 agent。
- readinessProbe：agent 连接上 API Server 并完成首次同步后才 Ready。健康检查端口使用 host 网络上的 `agent.healthPort`（默认 9790，避开 cilium-agent 占用的 9890）。controller 用 agent Pod 的 Ready 状态判断节点能否作为新主网关，不需要单独的心跳。

### 5.3 RBAC

| 资源 | controller | agent |
|---|---|---|
| `coordination.k8s.io/leases`（本命名空间） | get, create, update（选工作实例） | - |
| `egressgateway-ha.spidernet.io/egressgatewayhas` | get, list, watch, create（初次部署时创建）, update（写入 `spec.activeNode`） | get, list, watch |
| `egressgateway-ha.spidernet.io/egressgatewayhas/status` | patch（Server-Side Apply） | patch（Server-Side Apply，只写 `status.activeNode` 和 `GatewayReady` condition） |
| `nodes` | get, list, watch, patch | get（判断本节点是否仍匹配 `candidateNodeSelector`） |
| `cilium.io/ciliumnodes` | get, list, watch, patch | get, patch（只更新本节点的 annotation） |
| `cilium.io/ciliumegressgatewaypolicies` | get, list, watch | get, list, watch |
| `pods`（cilium 和本项目的命名空间） | get, list, watch（判断 cilium-agent 和 agent 是否 Ready） | - |
| `secrets`（本命名空间） | get, create, update（webhook 证书） | - |
| `mutatingwebhookconfigurations` / `validatingwebhookconfigurations` | get, patch（写入 caBundle） | - |
| `events` | create | - |

## 6. 方案选择

### 6.1 为什么由集中的 controller 决定主备

出口 IP 配置好以后，转发只依赖两样东西：主网关网卡上的 IP，以及 Cilium 的 BPF map，和本项目的进程、API Server 都没有关系。所以本方案按以下原则设计：

- **只按节点本身的状态切换。** controller 用 TCP 探测和 Node Ready 状态判断主网关是否故障，不用"本项目进程是否存活"来代表节点能否转发。
- **agent 只执行，不做决定。** agent 重启或升级时不删除出口 IP，已经生效的数据面保持不变。
- **只有一个地方做决定。** 决策集中在 controller 的工作实例上，候选节点之间不需要互斥，也不会出现多个节点各自判断后同时接管。

### 6.2 为什么不使用多节点匹配

多节点匹配方案的做法是：policy 的 nodeSelector 只匹配候选节点，Cilium 按节点名排序后选第一个作为主节点。主节点故障时，由某个组件删除它的 label，Cilium 就会选择下一个节点。

这个方案能实现切换，但还要回答三个问题：谁来判断主节点故障？由谁来执行切换？怎样避免两个节点同时持有出口 IP？把这些问题都解决后，就需要一个统一做决定的组件。

| 问题 | 多节点匹配 | 集中 controller（本方案） |
|---|---|---|
| 谁是主节点 | 由 Cilium 按节点名排序决定。程序要自己复现 Cilium 的选择规则，才知道出口 IP 应该配到哪个节点。这依赖 Cilium 的内部实现 | 由 controller 决定。policy 只匹配一个节点，所以出口 IP 的位置和 Cilium 的选择总是一致 |
| 故障检测 | 需要备节点自己判断。如果监听 Node Ready，默认至少要 40s | controller 用 TCP 连接探测节点，约 16～21s；Node NotReady 也触发切换 |
| 误判时是否双主 | **会。** 备节点加上出口 IP 后，旧主节点并不知道自己已被替换，还继续持有 IP，流量全部中断 | 新主节点添加 IP 前先做重复地址检测，发现冲突就不添加，也不修改 label |
| 多个备节点之间的互斥 | 需要另外加一把锁 | 只有一个 controller 实例做决定 |
| 故障节点恢复后 | 候选 label 已经被删除，需要运维手动加回 | 候选节点由 `candidateNodeSelector` 决定，节点恢复后自动成为备节点 |
| 是否强制切回 | **会。** 候选 label 加回后，原主节点名字排序最靠前，Cilium 立即切回，但出口 IP 还在备节点上，流量中断 | 不会。只有 `activeNode` 节点带有 active label |

### 6.3 为什么不在每个节点上预先配置出口 IP

- 出口 IP 配在普通网卡上时，多个节点会同时响应 ARP，回包可能被送到备节点并被丢弃。实测流量全部失败。
- 出口 IP 配在 `lo` 上，并设置 `arp_ignore=1` 时，没有任何节点响应 ARP。上游需要通过静态路由或 BGP 把 IP 指向当前主节点。这只是把"迁移 IP"换成了"切换路由"，在实际网络中很难落地。

### 6.4 与 MetalLB L2 模式的对比

MetalLB L2 模式也是"一个 IP 在多个节点之间主备切换"，以下内容基于 MetalLB 源码（`speaker/layer2_controller.go`、`internal/speakerlist/speakerlist.go`，commit `6be0662`）和官方文档 `concepts/layer2.md`。

| 方面 | MetalLB L2 | 本方案 |
|---|---|---|
| 选主方式 | **无状态、分布式**。每个 speaker 都收集同一组可用节点，按 `sha256(节点名 + "#" + IP)` 排序（可以用 `preferredNodes` 加权），排第一的节点宣告这个 IP。没有 Lease，也没有中心组件 | 中心化。controller 决定主网关并写入 `spec.activeNode`，选择结果有状态、不随节点增减而变化 |
| 可用节点 | memberlist 中存活的 speaker，排除带 `NetworkUnavailable` 条件或 `node.kubernetes.io/exclude-from-external-load-balancers` label 的节点，再按 L2Advertisement 的节点选择器过滤 | 候选节点中：TCP 探测可达、Node Ready、cilium-agent Ready、agent Pod Ready |
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
