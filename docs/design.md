# cilium-egressgateway-ha 设计文档

## 1. 背景

Cilium 社区版的 Egress Gateway 可以把匹配的 Pod 流量转发到指定网关节点，并 SNAT 成固定的出口 IP（egressIP）。但它**不提供网关高可用**：

- 网关节点宕机后，流量不会切换到其他节点。
- Cilium 不会配置或迁移 egressIP（VIP）。

本项目通过一个运行在候选网关节点上的 DaemonSet，实现网关故障自动切换和 VIP 管理。

## 2. Cilium 社区版行为分析

以下结论基于 Cilium 源码（`pkg/egressgateway`）阅读，并在 kind 集群（Cilium v1.20.5，tunnel/vxlan，KPR）中验证。

### 2.1 网关节点选择

- 每个 cilium-agent 根据 policy 的 `egressGateway.nodeSelector` 匹配 **CiliumNode** 的 labels。
- 多个节点同时匹配时，按节点名排序取第一个。
- 选择时**不检查**节点 Ready 状态，也不检查 agent 是否存活，没有 health check。
- 节点宕机后，它的 CiliumNode 仍然存在，labels 也不变。Cilium 会继续选择它，导致流量黑洞。operator 只会在 k8s Node 对象被删除后，才回收对应的 CiliumNode。

### 2.2 CiliumNode labels 的来源

- CiliumNode 的 labels 由**该节点自己的 cilium-agent** 从 k8s Node 同步过去，operator 不负责同步。
- 节点正常时，修改 k8s Node 的 label 会自动同步到 CiliumNode，所有 agent 都能感知，**不需要重启 agent**。
- 节点宕机后，它的 agent 也停止了。这时修改 k8s Node 的 label **不会同步**到 CiliumNode，必须直接修改 CiliumNode。
- 节点恢复后，agent 会按 k8s Node 的 labels 覆盖 CiliumNode 的 labels。
- 如果配置了 `exclude-node-label-patterns`，匹配的 label 不会同步到 CiliumNode。

### 2.3 egressIP 的查找时机

- Cilium **不会**把 egressIP 配置到网卡上。它只在网关节点上查找"哪块网卡拥有这个 IP"（或者这个 IP 是否被配置为 local route）。查找成功后，把 egressIP 写入 BPF egress map。
- 查找失败时，egressIP 被写成 `0.0.0.0`，网关节点会以 `No Egress IP configured` 的原因丢弃流量。
- 查找**由事件触发**。只有以下事件会让所有 agent 重新计算网关配置（`reconcileLocked` → `regenerateGatewayConfigs`）：
  - policy 新增、修改或删除
  - **任意** CiliumNode 新增、更新或删除
  - agent 启动后首次同步完成
- **网卡 IP 的增删不会触发重新查找。** 查找失败后，即使把 VIP 加到网卡上，流量也不会自动恢复。官方文档建议重新 apply policy。
- 官方文档默认的使用前提是：egressIP 由运维**预先**配置在固定的网关节点上，然后再创建 policy。

### 2.4 VIP 冲突

- 同一个二层网络中，如果多个节点同时持有 VIP，外部设备通过 ARP 找到哪个节点是不确定的。回包可能被送到备节点。备节点上没有这条连接的 SNAT 状态，回包会被丢弃。实测 6/6 次请求全部失败。
- 把 VIP 配到 `lo` 上也不能解决问题：默认 `arp_ignore=0` 时，`eth0` 仍然会响应 VIP 的 ARP。
- 因此，**任何时刻只能有一个节点持有 VIP**。

### 2.5 验证结果汇总

| 场景 | 结果 |
|---|---|
| 主节点在线，去掉 Node label，VIP 未迁移 | gatewayIP 自动切到备节点，但备节点上没有 VIP，流量被丢弃 |
| 在上一场景的基础上再迁移 VIP | 不会自动恢复，需要重建 policy |
| 先迁移 VIP，再修改 label | 立即生效 |
| 主节点宕机，不做任何处理 | 流量黑洞 |
| 主节点宕机，去掉 Node label | 不会同步到 CiliumNode，流量不恢复 |
| 主节点宕机，去掉 CiliumNode label（VIP 已迁移） | 立即恢复 |
| 两个节点同时持有 VIP | 流量全部失败 |
| VIP 晚于 policy 生效，然后 patch 本节点 CiliumNode 的 annotation | 立即恢复 |
| VIP 晚于 policy 生效，然后给 Node 打非 `cilium.io` 前缀的 annotation | 不会同步，流量不恢复 |
| VIP 晚于 policy 生效，然后给 Node 打 `xxx.cilium.io/` 前缀的 annotation | 同步到 CiliumNode，流量恢复 |

## 3. 目标与非目标

**目标**

- 主网关节点故障时，自动把出口流量和 VIP 切换到备节点。
- 任何时刻只有一个节点持有 VIP，并且只有一个节点被 Cilium 选为网关。
- 不修改 Cilium，只使用社区版现有的行为。

**非目标**

- 切换时保持已有连接不断开。旧网关上的 SNAT 状态不会迁移，已有连接会断开。
- 在 API Server 不可用时完成切换。
- 多个网关之间的负载均衡。

## 4. 总体设计

### 4.1 节点 label

| label | 维护者 | 含义 |
|---|---|---|
| `egress-gw-candidate=true` | 运维手动打，固定不变 | 候选网关节点，DaemonSet 只运行在这些节点上 |
| `egress-gw-active=true` | 程序自动维护 | 当前主网关。任何时刻最多只有一个节点有这个 label |

### 4.2 Egress policy

policy 的 nodeSelector 要**同时**匹配这两个 label。这样即使 `egress-gw-active` 被误打到非候选节点上，Cilium 也不会选中它。

```yaml
apiVersion: cilium.io/v2
kind: CiliumEgressGatewayPolicy
metadata:
  name: egress-app-a
spec:
  selectors:
  - podSelector:
      matchLabels:
        io.kubernetes.pod.namespace: app-a
        app: backend
  destinationCIDRs:
  - "0.0.0.0/0"
  excludedCIDRs:
  - "10.0.0.0/8"
  egressGateway:
    nodeSelector:
      matchLabels:
        egress-gw-candidate: "true"
        egress-gw-active: "true"
    egressIP: 192.168.10.100
```

程序只管理 nodeSelector 中包含 `egress-gw-active` 的 policy，并把这些 policy 的 IPv4 `egressIP` 作为 VIP。所有被管理的 policy 共用同一个主网关，切换时所有 VIP 一起迁移。

### 4.3 选主

- 候选节点上的每个 Pod 通过同一个 `coordination.k8s.io/Lease` 选主，可以使用 client-go 的 `leaderelection`。
- 所有候选节点平等，不设首选节点；原主节点恢复后不会主动切回。
- identity 使用节点名。
- 建议参数：`LeaseDuration=15s`、`RenewDeadline=10s`、`RetryPeriod=2s`。必须满足 `LeaseDuration > RenewDeadline`，并留出余量。这样旧 leader 会先放弃 VIP，新 leader 之后才能抢到 Lease。
- 主节点宕机后，切换时间大约是 `LeaseDuration + RetryPeriod`，约 15～17s，再加上 Cilium 下发配置的时间（秒级）。

### 4.4 方案选择

#### 4.4.1 为什么不使用多节点匹配

多节点匹配方案的做法是：policy 的 nodeSelector 只匹配候选节点，Cilium 按节点名排序后选第一个作为主节点。主节点故障时，由某个组件删除它的 label，Cilium 就会选择下一个节点。

这个方案能实现切换，但真正使用时还要回答三个问题：

- 谁来判断主节点故障？
- 由谁来执行切换？
- 怎样避免两个节点同时持有 VIP？

如果要把这三个问题都解决，最后还是需要 Lease 这类机制。

| 问题 | 多节点匹配 | Lease 选主（本方案） |
|---|---|---|
| 谁是主节点 | 由 Cilium 按节点名排序决定。程序要自己复现 Cilium 的选择规则，才知道 VIP 应该配到哪个节点。这依赖 Cilium 的内部实现 | 由程序选主决定。policy 只匹配一个节点，所以 VIP 的位置和 Cilium 的选择总是一致 |
| 故障检测 | 备节点需要自己判断主节点是否故障。如果监听 Node Ready，默认至少要 40s 才能检测到 | 以 Lease 过期作为故障信号。检测时间可以配置，约 15s |
| 误判时是否双主 | **会。** 比如只有 kubelet 挂了，或者主节点连不上 API Server，但主节点的网络仍然正常。这时备节点删除主节点的 label 并加上 VIP，主节点却不知道自己已被替换，还继续持有 VIP。两个节点同时持有 VIP，流量会全部中断（见 2.4） | **不会。** 主节点无法续约时，会在 `RenewDeadline` 时主动删除 VIP。新主节点要等 `LeaseDuration` 过期后才能接管，两者在时间上错开 |
| 多个备节点之间是否需要互斥 | 需要。有 3 个及以上候选节点时，多个备节点可能同时执行切换，需要另外加一把锁 | 不需要。Lease 本身就是互斥锁，同一时间只有一个节点执行切换 |
| 故障节点恢复后 | 候选 label 已经被删除，需要运维手动加回，否则候选节点会越来越少 | 候选 label 一直保留，节点恢复后自动成为备节点 |
| 是否强制切回 | **会。** 候选 label 加回后，原主节点的名字排序仍然最靠前，Cilium 会立即切回该节点。但 VIP 还在备节点上，流量会中断。这和"故障节点恢复后不切回"的决策冲突 | 不会。只有当前 leader 带有 active label |

关于"主节点宕机后 VIP 无法迁移"：如果节点真的宕机，它已经无法响应 ARP，新主节点直接添加 VIP 并发送 GARP 即可，两个方案在这里没有区别。真正的风险是**误判**：主节点看起来故障了，但实际上还活着。多节点匹配方案没有办法让旧主节点主动放弃 VIP；Lease 方案可以让旧主节点在续约失败后自我隔离。

所以，如果给多节点匹配补上故障检测、多个备节点之间的互斥，以及旧主节点的自我隔离，它最后就会变成 Lease 方案。剩下的区别只在于 label 的维护方式：多节点匹配需要反复删除和恢复候选 label，还会受到 Cilium 名字排序规则的影响；Lease 方案只切换一个 active label，候选节点集合不变。

#### 4.4.2 为什么不在每个节点上预先配置 VIP

把 VIP 预先配置在所有候选节点上，切换时就不需要迁移 VIP。但这个方案不可行：

- VIP 配在普通网卡上时，多个节点会同时响应 ARP，回包可能被送到备节点并被丢弃。实测流量全部失败（见 2.4）。
- VIP 配在 `lo` 上，并设置 `arp_ignore=1` 时，没有任何节点响应 ARP。上游需要通过静态路由或 BGP，把 VIP 指向当前主节点。这只是把"迁移 VIP"换成了"切换路由"，而且还要保证路由和 Cilium 选中的网关一致。在实际网络中很难落地。

## 5. 流程

### 5.1 Pod 启动

1. **无条件**删除本机网卡上所有被管理的 VIP。
2. 删除本节点 Node 上的 `egress-gw-active` label。
3. 参与 Lease 选主。

这里不需要先判断自己是不是主节点：刚启动的 Pod 不会是 leader。

### 5.2 成为 leader（`OnStartedLeading`）

顺序很重要：**先添加 VIP，再打 label。**

1. 读取需要管理的 policy，得到 VIP 列表。
2. 把 VIP 以 /32 添加到指定网卡上，然后发送 GARP。
3. 给**本节点 Node** 打上 `egress-gw-active=true`。本机 agent 正常运行，label 会自动同步到 CiliumNode，并触发 Cilium 重新查找 egressIP。这时 VIP 已经在网卡上，可以一次查找成功。
4. 查找其他带有 `egress-gw-active=true` 的节点，删除它们 **Node 和 CiliumNode** 上的这个 label。旧 leader 可能已经宕机，它的 agent 不会同步 Node 上的 label 变化，所以 CiliumNode 必须单独处理。

### 5.3 作为 leader 运行

- 监听 policy 变化：
  - 新增 egressIP 时：添加 VIP，发送 GARP，然后 **patch 本节点 CiliumNode 的 annotation**，例如 `egress-gw-ha/vip-synced=<timestamp>`，触发 Cilium 重新查找 egressIP。原因是 Cilium 收到 policy 事件时 VIP 还没加上，查找会失败，而且不会自动重试。
  - 删除 egressIP 时：删除对应的 VIP。
- 定期 reconcile，例如每 10s 一次：
  - 检查 VIP 是否还在网卡上。如果被 NetworkManager 或 systemd-networkd 等组件删除，就重新添加，然后 patch annotation。
  - 检查是否只有本节点有 `egress-gw-active`。
- annotation 只用来触发事件，不作为持久状态使用。agent 会覆盖 CiliumNode 的 annotations。
- 任意 CiliumNode 变化都会让所有 agent 重新计算网关配置，所以只在 VIP 变化后触发，不要频繁 patch。

### 5.4 失去 leader 身份（`OnStoppedLeading`）

这种情况通常是因为 Lease 无法续约，例如本节点连不上 API Server。

1. **立即删除本机所有 VIP。** 这一步只操作本机网卡，不依赖 API Server。
2. 尽量删除本节点的 `egress-gw-active` label。失败也没关系，新 leader 会删除。
3. 退出进程，由 kubelet 重启 Pod 后进入 5.1 流程。

### 5.5 收到 SIGTERM（滚动升级、驱逐、节点维护）

1. 删除本机 VIP。
2. 删除本节点 Node 上的 `egress-gw-active` label。
3. 释放 Lease（`ReleaseOnCancel=true`），让备节点尽快接管。

### 5.6 主节点宕机

| 时间 | 事件 |
|---|---|
| t0 | 主节点宕机，Pod、agent 和 VIP 都失效，CiliumNode label 仍然存在，流量黑洞 |
| t0 + ~15s | Lease 过期，备节点抢到 Lease，执行 5.2 流程 |
| t0 + ~16s | 备节点添加 VIP 并发送 GARP，给自己打 label，删除旧主节点 Node 和 CiliumNode 上的 label |
| t0 + ~17s | 所有 agent 把 gatewayIP 切到新主节点，新建连接恢复 |
| 旧主节点恢复 | Pod 启动后执行 5.1，确认没有 VIP。agent 按 Node labels 覆盖 CiliumNode，此时已经没有 active label，因此不会抢回主网关角色 |

## 6. 部署

### 6.1 DaemonSet 要求

- `nodeSelector: egress-gw-candidate: "true"`
- `hostNetwork: true`
- capabilities：`NET_ADMIN`（增删 IP）、`NET_RAW`（发送 GARP）
- `terminationGracePeriodSeconds` 要留出足够时间，用于删除 VIP 和释放 Lease。

### 6.2 RBAC

| 资源 | 动作 |
|---|---|
| `coordination.k8s.io/leases`（本命名空间） | get, create, update |
| `nodes` | get, list, watch, patch |
| `cilium.io/ciliumnodes` | get, list, watch, patch |
| `cilium.io/ciliumegressgatewaypolicies` | get, list, watch |

### 6.3 配置参数

| 参数 | 说明 |
|---|---|
| `--interface` | VIP 所在网卡。不指定时，自动选择和 VIP 同网段的网卡 |
| `--candidate-label` / `--active-label` | label key，默认 `egress-gw-candidate` / `egress-gw-active` |
| `--lease-name` / `--lease-namespace` | Lease 对象的名称和命名空间 |
| `--lease-duration` / `--renew-deadline` / `--retry-period` | 选主参数 |

## 7. 限制与风险

- **已有连接会中断**：切换网关时，旧网关上的 SNAT 状态会丢失，已有 TCP 长连接需要应用重新连接。
- **依赖 API Server**：API Server 不可用时，leader 无法续约，会主动删除 VIP，导致出口中断。这是为了避免双主而做的保守取舍。
- **时钟偏差**：Lease 依赖各节点时钟大致同步，需要部署 NTP。
- **GARP**：上游交换机或路由器必须接受 GARP，并及时更新 ARP 表。
- **外部组件冲突**：节点上的网络管理组件可能会删除程序添加的 VIP。程序通过定期 reconcile 恢复 VIP，但仍然要避免这类组件管理该网卡。
- **Cilium 行为依赖**：本设计依赖 Cilium 的内部行为，例如 CiliumNode 更新会触发重新查找 egressIP。升级 Cilium 前需要重新验证。

## 8. 设计决策

1. **不设首选主节点**：所有候选节点平等，完全按照 Lease 选主结果决定主节点。原主节点恢复后作为备节点运行，不主动切回。
2. **共用一个 Lease 和一个 active label**：所有被管理的 policy 共用同一个主网关。切换时，所有 VIP 一起迁移到新 leader。
3. **只支持 IPv4**：只管理 `egressIP` 为 IPv4 地址的 policy。`egressIP` 为 IPv6、或者未指定 `egressIP`（使用 `interface` 或默认路由网卡）的 policy 会被忽略，并输出日志。
