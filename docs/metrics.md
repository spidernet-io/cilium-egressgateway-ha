# Metrics

controller 和 agent 都提供 Prometheus metrics，名字都以 `egressgateway_ha_` 开头。本文列出每个 metric 的含义和用法，并给出常用的告警规则。

## 1. 采集方式

| 组件 | 端口 | Service | 说明 |
|---|---|---|---|
| controller | `controller.metricsPort`，默认 8080 | `egwha-controller-metrics`（headless） | 只有拿到 Lease 的工作实例会更新 metrics，另一个副本没有这些数据 |
| agent | `agent.metricsPort`，默认 9791 | `egwha-agent-metrics`（headless） | agent 使用 host 网络，端口在节点上监听；每个出口节点一个 agent |

安装了 Prometheus Operator 时，可以让 chart 创建 ServiceMonitor：

```yaml
serviceMonitor:
  enabled: true
  interval: 30s
  labels: {}        # 按 Prometheus 的 serviceMonitorSelector 填写
```

手动查看：

```console
$ kubectl -n kube-system get lease egressgateway-ha-controller -o jsonpath='{.spec.holderIdentity}'
egwha-controller-7cd84d567f-92dsb_...
$ kubectl get --raw "/api/v1/namespaces/kube-system/pods/egwha-controller-7cd84d567f-92dsb:8080/proxy/metrics" | grep ^egressgateway_ha
egressgateway_ha_active_node{node="egw-e2e-worker"} 1
egressgateway_ha_active_node_component_ready{component="agent"} 1
egressgateway_ha_active_node_component_ready{component="cilium-agent"} 1
egressgateway_ha_condition{reason="AsExpected",status="True",type="Ready"} 1
egressgateway_ha_condition{reason="Completed",status="False",type="Switching"} 1
egressgateway_ha_condition{reason="EgressIPsConfigured",status="True",type="GatewayReady"} 1
egressgateway_ha_condition{reason="Initial",status="True",type="ActiveNodeElected"} 1
egressgateway_ha_condition{reason="KubeletReady",status="True",type="ActiveNodeReady"} 1
egressgateway_ha_condition{reason="ProbeSucceeded",status="True",type="ActiveNodeReachable"} 1
egressgateway_ha_egress_node_available{node="egw-e2e-worker"} 1
egressgateway_ha_egress_node_available{node="egw-e2e-worker2"} 1
egressgateway_ha_egress_node_reachable{node="egw-e2e-worker"} 1
egressgateway_ha_egress_node_reachable{node="egw-e2e-worker2"} 1

# 在主网关节点上执行
$ curl -s localhost:9791/metrics | grep ^egressgateway_ha
egressgateway_ha_agent_egress_ip{interface="eth0",ip="172.21.255.195"} 1
```

counter 类型的 metric（`switches_total`、`agent_dad_conflicts_total`）在第一次计数之前不会出现，上面的输出中就没有它们。进程重启后 counter 从 0 开始，查询时请用 `increase()` 或 `rate()`。

## 2. controller metrics

| metric | 类型 | label | 含义 |
|---|---|---|---|
| `egressgateway_ha_active_node` | gauge | `node` | 当前主网关（`spec.activeNode`）为 1。没有主网关时没有数据 |
| `egressgateway_ha_switches_total` | counter | `reason` | 开始的切换次数。reason 为 `Initial`（初次选主）、`Failover`（故障切换）、`Manual`（手动切换）、`EgressNodeRemoved`（主网关不再是出口节点） |
| `egressgateway_ha_condition` | gauge | `type`、`status`、`reason` | `EgressGatewayHA` 每个 condition 当前的 status 和 reason 为 1，每个 type 只有一条数据。和 `kubectl get egressgatewayha default -o yaml` 中的 conditions 一致 |
| `egressgateway_ha_egress_node_reachable` | gauge | `node` | 出口节点最近一次 TCP 探测成功为 1，失败为 0。关闭 `controller.egressNodeProbe.enabled` 时没有数据 |
| `egressgateway_ha_egress_node_available` | gauge | `node` | 出口节点可以成为主网关为 1。需要同时满足：TCP 探测成功、Node Ready、有 CiliumNode、cilium-agent Ready（`controller.ciliumAgentCheck.enabled` 时）、agent Pod Ready |
| `egressgateway_ha_active_node_component_ready` | gauge | `component` | 主网关上的组件就绪为 1。component 为 `agent` 和 `cilium-agent`（`controller.ciliumAgentCheck.enabled` 时）。组件未就绪不会触发切换，只用于告警 |

`condition` 的常见取值：

| type | 正常 | 需要关注 |
|---|---|---|
| `Ready` | `True/AsExpected` | `False`，reason 为第一个异常的 condition type |
| `Switching` | `False/Completed` | `True/NoAvailableEgressNode`：主网关故障但没有可切换的节点；`True/EgressIPConflict`：新主网关上的出口 IP 被其他设备占用；`True/WaitingForGateway` 长时间不结束 |
| `ActiveNodeReachable` | `True/ProbeSucceeded` | `False/ProbeFailed` |
| `ActiveNodeReady` | `True/KubeletReady` | `False` 或 `Unknown` |
| `GatewayReady` | `True/EgressIPsConfigured` 或 `True/NoManagedPolicy` | `False/EgressIPConflict`、`False/InterfaceNotFound`、`False/ConfigureFailed` |

## 3. agent metrics

| metric | 类型 | label | 含义 |
|---|---|---|---|
| `egressgateway_ha_agent_egress_ip` | gauge | `interface`、`ip` | 本节点网卡上由本项目管理的出口 IP，每个 IP 一条，值为 1。正常情况下只有主网关有数据 |
| `egressgateway_ha_agent_dad_conflicts_total` | counter | `ip` | 添加出口 IP 前做重复地址检测，发现其他设备已经在使用这个 IP 的次数 |

## 4. 告警规则示例

```yaml
groups:
- name: egressgateway-ha
  rules:
  # 汇总状态异常，例如主网关不可达、出口 IP 未配置、没有可用节点
  - alert: EgressGatewayHANotReady
    expr: egressgateway_ha_condition{type="Ready",status="False"} == 1
    for: 1m
    annotations:
      summary: "EgressGatewayHA is not ready: {{ $labels.reason }}"

  # 主网关故障，但没有可以切换的出口节点
  - alert: EgressGatewayHANoAvailableEgressNode
    expr: egressgateway_ha_condition{type="Switching",reason="NoAvailableEgressNode"} == 1
    annotations:
      summary: "No available egress node to switch to"

  # 发生了故障切换
  - alert: EgressGatewayHAFailover
    expr: increase(egressgateway_ha_switches_total{reason="Failover"}[10m]) > 0
    annotations:
      summary: "The active node failed over"

  # 可用的出口节点少于 2 个，主网关再故障时无法切换
  - alert: EgressGatewayHALowRedundancy
    expr: sum(egressgateway_ha_egress_node_available) < 2
    for: 5m
    annotations:
      summary: "Less than 2 available egress nodes"

  # 同一个出口 IP 出现在多个节点上（例如旧主网关失联后仍持有 IP）
  - alert: EgressGatewayHADuplicateEgressIP
    expr: count by (ip) (egressgateway_ha_agent_egress_ip) > 1
    for: 1m
    annotations:
      summary: "Egress IP {{ $labels.ip }} is configured on more than one node"

  # 主网关上的 agent 或 cilium-agent 长时间未就绪，不影响当前流量，但影响后续配置
  - alert: EgressGatewayHAActiveNodeComponentNotReady
    expr: egressgateway_ha_active_node_component_ready == 0
    for: 5m
    annotations:
      summary: "{{ $labels.component }} on the active node is not ready"
```

切换的历史还可以通过 Event 查看：`kubectl get events --field-selector involvedObject.kind=EgressGatewayHA`。Event 默认保留 1 小时，controller 日志中也有对应的 INFO 记录。
