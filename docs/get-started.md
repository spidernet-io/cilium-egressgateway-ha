# 快速开始

本文介绍如何安装 cilium-egressgateway-ha，创建一个由它管理的 egress policy，并验证出口 IP 和主备切换。工作原理见[设计文档](design.md)。

文中的输出来自一个 4 节点的示例集群：

| 项目 | 值 |
|---|---|
| 出口节点 | `egw-e2e-control-plane`、`egw-e2e-worker`、`egw-e2e-worker2` |
| 业务节点 | `egw-e2e-worker3` |
| 节点网段 | `172.21.0.0/16`，网卡 `eth0` |
| 出口 IP | `172.21.255.195-172.21.255.205` |
| 集群外的服务 | `172.21.255.250:8080`，返回请求的源地址 |

实际使用时，替换成自己环境中的节点名和 IP。

## 1. 前提条件

**集群**

- Kubernetes v1.30 或更高版本。webhook 使用了 `matchConditions`，这个功能在 v1.30 GA。
- 已经安装 Cilium，并开启 egress gateway。以下是 e2e 测试中使用的配置，已经验证过：

  ```bash
  helm upgrade --install cilium cilium --repo https://helm.cilium.io -n kube-system \
    --reuse-values \
    --set kubeProxyReplacement=true \
    --set bpf.masquerade=true \
    --set egressGateway.enabled=true
  ```

  修改后需要重启 cilium-agent 和 cilium-operator 才能生效：

  ```bash
  kubectl -n kube-system rollout restart ds/cilium deploy/cilium-operator
  ```

**网络**

- 准备一个或一段**空闲**的 IPv4 地址作为出口 IP。它们必须和出口节点的网卡在同一个二层网段中，并且不能被任何设备占用。
- 上游交换机或路由器需要接受 GARP（免费 ARP），切换后才能及时更新 ARP 表。
- 至少准备 2 个出口节点，生产环境建议 3 个。

**工具**

- `kubectl`
- `helm`

## 2. 选择出口节点

给可以持有出口 IP 的节点打上 label：

```console
$ kubectl label node egw-e2e-control-plane egw-e2e-worker egw-e2e-worker2 egressgateway-ha.spidernet.io/egress-node=true
node/egw-e2e-control-plane labeled
node/egw-e2e-worker labeled
node/egw-e2e-worker2 labeled
```

这些节点就是出口节点。主网关会从中选出，agent 也只会运行在这些节点上。`egressgateway-ha.spidernet.io/egress-node=true` 是 `global.egressNodeSelector` 的默认值；如果想用节点上已有的 label，可以在安装时修改 `global.egressNodeSelector`。

## 3. 安装

添加 Helm 仓库：

```bash
helm repo add egressgateway-ha https://spidernet-io.github.io/cilium-egressgateway-ha
helm repo update egressgateway-ha
```

准备 values 文件 `my-values.yaml`：

```yaml
egressGatewayHA:
  # 出口 IP，支持单个 IP 和 "起始IP-结束IP"，必填
  egressIPs:
  - 172.21.255.195-172.21.255.205
```

安装最新版本的 chart：

```console
$ helm install egwha egressgateway-ha/cilium-egressgateway-ha -n kube-system -f my-values.yaml --wait
NAME: egwha
LAST DEPLOYED: Wed Nov 25 10:57:44 2026
NAMESPACE: kube-system
STATUS: deployed
REVISION: 1
TEST SUITE: None
NOTES:
cilium-egressgateway-ha is installed in kube-system.
...
```

完整配置可以用 `helm show values egressgateway-ha/cilium-egressgateway-ha` 查看。几个常用的配置：

| 配置 | 默认值 | 说明 |
|---|---|---|
| `controller.egressNodeProbe.enabled` | `true` | 对出口节点做 TCP 探测。关闭后，节点网络故障不会触发切换 |
| `controller.ciliumAgentCheck.enabled` | `true` | 选择新主网关时，要求节点上的 cilium-agent Ready |
| `ciliumEgressPolicyWebhook.enabled` | `true` | 补全和校验 policy。关闭后，未填写 egressIP 的 policy 不会被补全 |
| `global.egressNodeSelector` | `egressgateway-ha.spidernet.io/egress-node: "true"` | 选择出口节点。可以改用节点上已有的 label |
| `global.activeNodeLabel.key`、`.value` | `egressgateway-ha.spidernet.io/active-node`、`"true"` | 主网关 label，只能在安装时设置 |
| `egressGatewayHA.interface` | `""` | 出口网卡。为空时，agent 选择子网包含出口 IP 的网卡，找不到时用默认路由的网卡 |
| `global.imageRegistryOverride` | `""` | 统一替换所有镜像的 registry，适合离线环境 |

> `egressGatewayHA` 部分只在 controller 首次创建 `EgressGatewayHA default` 时使用。之后修改配置需要直接修改这个 CR，`helm upgrade` 不会覆盖它。

## 4. 检查安装结果

controller 有 2 个副本，每个出口节点上有 1 个 agent：

```console
$ kubectl -n kube-system get pods -l app.kubernetes.io/instance=egwha -o wide
NAME                                READY   STATUS    RESTARTS   AGE   IP             NODE                    NOMINATED NODE   READINESS GATES
egwha-agent-fkhhg                   1/1     Running   0          27s   172.21.0.2     egw-e2e-control-plane   <none>           <none>
egwha-agent-nr2ck                   1/1     Running   0          27s   172.21.0.4     egw-e2e-worker2         <none>           <none>
egwha-agent-xdn6d                   1/1     Running   0          27s   172.21.0.5     egw-e2e-worker          <none>           <none>
egwha-controller-7bf74795c6-fzbhj   1/1     Running   0          27s   10.244.0.90    egw-e2e-control-plane   <none>           <none>
egwha-controller-7bf74795c6-nbpg7   1/1     Running   0          27s   10.244.0.106   egw-e2e-control-plane   <none>           <none>
```

controller 创建了 `EgressGatewayHA default`，并选出了主网关：

```console
$ kubectl get egressgatewayha
NAME      ACTIVE           REACHABLE   READY   AGE
default   egw-e2e-worker   True        True    26s
```

- `ACTIVE`：当前主网关。
- `REACHABLE`：主网关的 TCP 探测结果。
- `READY`：主网关正常，并且出口 IP 已经配置好。

此时还没有被管理的 policy，所以主网关上还没有配置出口 IP，`GatewayReady` 的 reason 为 `NoManagedPolicy`。主网关会提前选出，这样 policy 创建后马上就有网关可用；否则在选主完成前，Cilium 找不到网关，会丢弃 Pod 的出口流量。

## 5. 创建 egress policy

policy 的 `nodeSelector` 写成 `egressgateway-ha.spidernet.io/active-node: "true"`，就会由本项目管理。下面的 policy 把 `app-a` 命名空间中 Pod 访问集群外服务的流量，经主网关 SNAT 成出口 IP：

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
  destinationCIDRs:
  - 172.21.255.250/32
  egressGateway:
    nodeSelector:
      matchLabels:
        egressgateway-ha.spidernet.io/active-node: "true"
    # egressIP 可以不写，webhook 会填入 defaultEgressIP；
    # 未设置 defaultEgressIP 时，使用 egressIPs 中的第一个
```

```console
$ kubectl apply -f egress-app-a.yaml
ciliumegressgatewaypolicy.cilium.io/egress-app-a created

$ kubectl get ciliumegressgatewaypolicy egress-app-a -o jsonpath='{.spec.egressGateway.egressIP}{"\n"}'
172.21.255.195
```

注意：

- `nodeSelector` 只能包含 `egressgateway-ha.spidernet.io/active-node: "true"`（主网关 label），不能有其他条件。
- 不能填写 `interface`。出口 IP 配在哪块网卡上，由 `EgressGatewayHA` 的 `spec.interface` 决定，默认自动选择。
- 填写 egressIP 时，它必须在 `egressIPs` 中。

## 6. 验证出口 IP

在主网关 `egw-e2e-worker` 上，出口 IP 以 /32 的形式配在网卡上，地址 label 为 `eth0:egw`：

```console
$ ip -4 addr show dev eth0
2: eth0@if251: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP group default  link-netnsid 0
    inet 172.21.0.5/16 brd 172.21.255.255 scope global eth0
       valid_lft forever preferred_lft forever
    inet 172.21.255.195/32 scope global eth0:egw
       valid_lft forever preferred_lft forever
```

从 `app-a` 中的 Pod 访问集群外的服务，服务端看到的源地址是出口 IP：

```console
$ kubectl -n app-a exec backend -- curl -s http://172.21.255.250:8080/clientip
172.21.255.195:43512
```

查看详细状态：

```console
$ kubectl get egressgatewayha default -o yaml
...
spec:
  activeNode: egw-e2e-worker
  egressIPs:
  - 172.21.255.195-172.21.255.205
  egressNodeSelector:
    matchLabels:
      egressgateway-ha.spidernet.io/egress-node: "true"
status:
  activeNode:
    egressIPs:
    - 172.21.255.195
    name: egw-e2e-worker
    ready: true
  conditions:
  - lastTransitionTime: "2026-11-25T10:57:46Z"
    message: 'egw-e2e-worker became active node: egw-e2e-worker selected as the initial
      active node'
    observedGeneration: 2
    reason: Initial
    status: "True"
    type: ActiveNodeElected
  - lastTransitionTime: "2026-11-25T10:57:46Z"
    message: probe to 172.21.0.5:10250 succeeded
    observedGeneration: 2
    reason: ProbeSucceeded
    status: "True"
    type: ActiveNodeReachable
  - lastTransitionTime: "2026-11-25T10:57:46Z"
    message: egw-e2e-worker is Ready
    observedGeneration: 2
    reason: KubeletReady
    status: "True"
    type: ActiveNodeReady
  - lastTransitionTime: "2026-11-25T10:57:46Z"
    message: switched <none> -> egw-e2e-worker
    observedGeneration: 2
    reason: Completed
    status: "False"
    type: Switching
  - lastTransitionTime: "2026-11-25T10:57:46Z"
    message: egw-e2e-worker is the active node and its egress IPs are configured
    observedGeneration: 2
    reason: AsExpected
    status: "True"
    type: Ready
  - lastTransitionTime: "2026-11-25T10:57:46Z"
    message: 1 egress IP(s) configured on eth0 (auto-detected) of egw-e2e-worker
    observedGeneration: 2
    reason: EgressIPsConfigured
    status: "True"
    type: GatewayReady
  egressNodes:
  - lastTransitionTime: "2026-11-25T10:57:46Z"
    name: egw-e2e-control-plane
    reachable: true
  - lastTransitionTime: "2026-11-25T10:57:46Z"
    name: egw-e2e-worker
    reachable: true
  - lastTransitionTime: "2026-11-25T10:57:46Z"
    name: egw-e2e-worker2
    reachable: true
```

- `status.activeNode`：主网关上已经配置的出口 IP。
- `status.egressNodes`：各出口节点的 TCP 探测结果。`lastTransitionTime` 是 `reachable` 最近一次变化的时间；探测失败时，`message` 写明原因。
- `status.conditions`：选主、探测、切换和出口 IP 的状态。出现问题时，先看 `Ready` 的 reason 和 message。

## 7. 主备切换

**故障切换**：主网关宕机、网络不可达或者 Node NotReady 时，controller 会自动把出口 IP 切到下一个可用的出口节点。故障节点恢复后不会自动切回。

**手动切换**：例如维护前，先把主网关切到 `egw-e2e-worker2`：

```console
$ kubectl patch egressgatewayha default --type merge -p '{"spec":{"activeNode":"egw-e2e-worker2"}}'
egressgatewayha.egressgateway-ha.spidernet.io/default patched

$ kubectl get egressgatewayha -w
NAME      ACTIVE           REACHABLE   READY   AGE
default   egw-e2e-worker   True        True    60s
default   egw-e2e-worker2   True        True    61s
default   egw-e2e-worker2   True        True    61s
default   egw-e2e-worker2   True        False   61s
default   egw-e2e-worker2   True        False   62s
default   egw-e2e-worker2   True        False   62s
default   egw-e2e-worker2   True        True    62s
```

`READY` 变为 `True`，说明切换完成，出口 IP 已经在新的主网关上。新建的连接仍然使用同一个出口 IP；切换时，已有的连接会断开。

```console
$ kubectl -n app-a exec backend -- curl -s http://172.21.255.250:8080/clientip
172.21.255.195:47760
```

目标节点必须是出口节点，否则会被拒绝：

```console
$ kubectl patch egressgatewayha default --type merge -p '{"spec":{"activeNode":"egw-e2e-worker3"}}'
Error from server (Forbidden): admission webhook "validate-egressgatewayha.egressgateway-ha.spidernet.io" denied the request: spec.activeNode: node egw-e2e-worker3 does not match spec.egressNodeSelector
```

查看切换记录：

```console
$ kubectl get events --field-selector involvedObject.kind=EgressGatewayHA
LAST SEEN   TYPE      REASON                  OBJECT                    MESSAGE
70s         Warning   NoAvailableEgressNode   egressgatewayha/default   no available egress node for the initial active node: [egw-e2e-worker: egressgateway-ha agent not ready, TCP probe not finished yet egw-e2e-worker2: egressgateway-ha agent not ready, TCP probe not finished yet egw-e2e-control-plane: egressgateway-ha agent not ready, TCP probe not finished yet]
69s         Normal    SwitchStarted           egressgatewayha/default   switching <none> -> egw-e2e-worker (Initial): egw-e2e-worker selected as the initial active node
69s         Normal    SwitchCompleted         egressgatewayha/default   egw-e2e-worker became active node: egw-e2e-worker selected as the initial active node
9s          Normal    SwitchStarted           egressgatewayha/default   switching egw-e2e-worker -> egw-e2e-worker2 (Manual)
8s          Normal    SwitchCompleted         egressgatewayha/default   egw-e2e-worker2 became active node, manual switch from egw-e2e-worker
```

刚安装时，agent 还没有就绪，可能会出现一条 `NoAvailableEgressNode` 告警，agent 就绪后会自动选出主网关。

## 8. 卸载

`helm uninstall` 只删除 controller、agent 和 webhook，不会中断出口流量：

```console
$ helm uninstall egwha -n kube-system --wait
release "egwha" uninstalled
```

出口 IP 和主网关 label 仍然保留在主网关上，出口流量不受影响：

```console
$ ip -4 addr show dev eth0 label eth0:egw        # 在主网关 egw-e2e-worker2 上执行
    inet 172.21.255.195/32 scope global eth0:egw
       valid_lft forever preferred_lft forever

$ kubectl get nodes -l egressgateway-ha.spidernet.io/active-node=true
NAME              STATUS   ROLES    AGE   VERSION
egw-e2e-worker2   Ready    <none>   24h   v1.36.1

$ kubectl -n app-a exec backend -- curl -s http://172.21.255.250:8080/clientip
172.21.255.195:47762
```

如果要彻底清理：

```console
# 1. 删除被管理的 policy，以及 EgressGatewayHA
$ kubectl delete ciliumegressgatewaypolicy egress-app-a
ciliumegressgatewaypolicy.cilium.io "egress-app-a" deleted
$ kubectl delete egressgatewayha default
egressgatewayha.egressgateway-ha.spidernet.io "default" deleted

# 2. 删除主网关上的 label。cilium-agent 会把 Node 的 label 同步到 CiliumNode，
#    如果 CiliumNode 上的 label 还在，再执行 kubectl label ciliumnode <节点> egressgateway-ha.spidernet.io/active-node-
$ kubectl label node egw-e2e-worker2 egressgateway-ha.spidernet.io/active-node-
node/egw-e2e-worker2 unlabeled

# 3. 在主网关上删除出口 IP
$ ip addr del 172.21.255.195/32 dev eth0

# 4. 删除 CRD 和证书 Secret
$ kubectl delete crd egressgatewayhas.egressgateway-ha.spidernet.io
customresourcedefinition.apiextensions.k8s.io "egressgatewayhas.egressgateway-ha.spidernet.io" deleted
$ kubectl -n kube-system delete secret egwha-webhook-cert
secret "egwha-webhook-cert" deleted from kube-system namespace
```
