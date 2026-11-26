# Cilium Egress Gateway 行为分析

本文是[设计文档](../design.md)的参考资料，记录本项目依赖的 Cilium 行为。

以下结论基于 Cilium 源码（`pkg/egressgateway`）阅读，并在 kind 集群（Kubernetes v1.36.1，Cilium v1.20.5，tunnel/vxlan，KPR）中验证。升级 Cilium 前需要重新验证。

## 1. 网关节点选择

- 每个 cilium-agent 根据 policy 的 `egressGateway.nodeSelector` 匹配 **CiliumNode** 的 labels。
- 多个节点同时匹配时，按节点名排序取第一个。
- 选择时**不检查**节点 Ready 状态，也不检查 agent 是否存活，没有 health check。
- 节点宕机后，它的 CiliumNode 仍然存在，labels 也不变。Cilium 会继续选择它，导致流量黑洞。operator 只会在 k8s Node 对象被删除后，才回收对应的 CiliumNode。

## 2. CiliumNode labels 的来源

- CiliumNode 的 labels 由**该节点自己的 cilium-agent** 从 k8s Node 同步过去，operator 不负责同步。
- 节点正常时，修改 k8s Node 的 label 会自动同步到 CiliumNode，所有 agent 都能感知，**不需要重启 agent**。
- 节点宕机后，它的 agent 也停止了。这时修改 k8s Node 的 label **不会同步**到 CiliumNode，必须直接修改 CiliumNode。
- 节点恢复后，agent 会按 k8s Node 的 labels 覆盖 CiliumNode 的 labels。
- 如果配置了 `exclude-node-label-patterns`，匹配的 label 不会同步到 CiliumNode。

所以本项目切换主网关时，会同时修改 Node 和 CiliumNode 上的 `egressgateway-ha.spidernet.io/active-node` label。

## 3. egressIP 的查找时机

- Cilium **不会**把 egressIP 配置到网卡上。它只在网关节点上查找"哪块网卡拥有这个 IP"（或者这个 IP 是否被配置为 local route）。查找成功后，把 egressIP 写入 BPF egress map。
- 查找失败时，egressIP 被写成 `0.0.0.0`，网关节点会以 `No Egress IP configured` 的原因丢弃流量。
- 查找**由事件触发**。只有以下事件会让所有 agent 重新计算网关配置（`reconcileLocked` → `regenerateGatewayConfigs`）：
  - policy 新增、修改或删除
  - **任意** CiliumNode 新增、更新或删除
  - agent 启动后首次同步完成
- **网卡 IP 的增删不会触发重新查找。** 查找失败后，即使把 VIP 加到网卡上，流量也不会自动恢复。官方文档建议重新 apply policy。
- 官方文档默认的使用前提是：egressIP 由运维**预先**配置在固定的网关节点上，然后再创建 policy。

所以本项目必须**先添加出口 IP，再打 label**。出口 IP 晚于 policy 生效时，agent 会 patch 本节点 CiliumNode 的 annotation，触发重新查找。

## 4. VIP 冲突

- 同一个二层网络中，如果多个节点同时持有 VIP，外部设备通过 ARP 找到哪个节点是不确定的。回包可能被送到备节点。备节点上没有这条连接的 SNAT 状态，回包会被丢弃。实测 6/6 次请求全部失败。
- 把 VIP 配到 `lo` 上也不能解决问题：默认 `arp_ignore=0` 时，`eth0` 仍然会响应 VIP 的 ARP。
- 因此，**任何时刻只能有一个节点持有 VIP**。

## 5. policy 的校验

- **Cilium 没有 admission webhook。** 安装 Cilium 后，集群中没有任何 mutating 或 validating webhook 配置。`CiliumEgressGatewayPolicy` 只靠 CRD 的 OpenAPI schema 校验。
- schema 中的必填字段：`selectors`、`destinationCIDRs`、`egressGateway`、`egressGateway.nodeSelector`。`egressIP` 和 `interface` 都可以不填，server dry-run 实测只写 nodeSelector 的 policy 可以创建。
- 两者都不填时，Cilium 使用网关节点默认路由网卡的 IP 作为出口 IP，不会报错。所以本项目必须由 webhook 补全 egressIP。
- **`egressIP` 和 `interface` 不能同时填写。** 同时填写时，API Server 接受，对象可以创建；但所有 cilium-agent 都解析失败并忽略这个 policy，日志为 `gateway configuration can't specify both an interface and an egress IP`。流量按没有 policy 处理，不报任何错误。
- **不存在和 Cilium webhook 的顺序问题。** Kubernetes 的处理顺序固定为：mutating admission → CRD schema 校验（包括必填字段）→ validating admission。kind 中用 MutatingAdmissionPolicy 验证过：用户连 `egressGateway` 整个字段都不写，被补全后可以成功创建。

## 6. NetworkUnavailable 条件

- 这个 Node condition 由云厂商的 route controller 或 CNI 设置。
- Cilium 只在 agent 启动时把它设为 False（reason `CiliumIsUp`），之后不再更新。
- kind 中实测，agent 重启、节点停机再启动后，这个 condition 的 `lastHeartbeatTime` 都没有变化。所以它不能反映运行时故障，本项目不用它判断节点是否故障。

## 7. 验证结果汇总

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
| 网关节点的 cilium-agent 重启（`kill -9`） | BPF map 保持不变，出口流量不受影响 |
| 新网关节点的 cilium-agent 不可用时，把 label 切到该节点 | 其他节点已经把网关切过来，但新网关节点本机没有下发 SNAT 配置，**流量持续中断**，直到该节点的 cilium-agent 恢复 |
| policy 同时填写 `egressIP` 和 `interface` | 对象可以创建，但所有 cilium-agent 都忽略这个 policy（见第 5 节） |
| 网关节点上的 VIP 被删除，label 不变 | Cilium 不感知（见第 3 节），BPF 仍然用 VIP 做 SNAT，回包按上游 ARP 缓存送回，所以在上游 ARP 表项过期前流量仍然正常 |
