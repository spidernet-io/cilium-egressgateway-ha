# 在本地运行 e2e 测试

e2e 测试验证的是**当前代码**：它在本地用 kind 创建一个集群，安装 Cilium，再从当前代码构建 controller 和 agent 镜像，用仓库中的 chart（`charts/cilium-egressgateway-ha`）安装后运行测试。测试不使用已发布的镜像和 chart。每个 PR 在 GitHub Actions 中执行的也是同样的流程（见 [ci.yaml](../.github/workflows/ci.yaml)）。

## 1. 准备环境

需要一台 Linux 机器，安装以下工具：

| 工具 | 说明 |
|---|---|
| docker | 运行 kind 节点和外部目标容器 |
| kind | 创建集群 |
| kubectl、helm | 安装 Cilium 和本项目 |
| go | 版本见 `go.mod`；版本较低时，go 会自动下载所需的 toolchain |
| make、python3、curl | 测试脚本使用 |

ginkgo 不需要单独安装，测试通过 `go tool ginkgo` 运行，版本固定在 `go.mod` 中。

kind 的每个节点都会占用宿主机的 inotify 实例。默认上限 128 不够时，创建集群会失败，报错类似 `could not find a log line that matches "Reached target .*Multi-User System.*"`。可以调大上限：

```bash
sudo sysctl -w fs.inotify.max_user_instances=512
sudo sysctl -w fs.inotify.max_user_watches=524288
```

## 2. 一键运行

```bash
make e2e
```

依次执行下面三步，第一次运行大约需要 15～20 分钟，其中测试本身约 8 分钟。

## 3. 分步运行

### 3.1 创建集群和测试环境

```bash
make e2e-cluster
```

- 创建 kind 集群 `egw-e2e`：1 个 control-plane 和 3 个 worker，使用单独的 docker 网络 `egw-e2e`。
- 安装最新版本的 Cilium chart，开启 egress gateway。需要指定版本时，设置 `CILIUM_VERSION`，例如 `make e2e-cluster CILIUM_VERSION=1.20.5`。
- 给 control-plane、worker、worker2 打上 `egressgateway-ha.spidernet.io/egress-node=true`（chart 中 `global.egressNodeSelector` 的默认值），作为出口节点。
- 在 worker3 上运行 client Pod，作为出口流量的来源。
- 在 kind 网络中启动外部目标容器 `egw-e2e-target`。
- 从 kind 网络的网段中选出目标 IP 和出口 IP 段，连同节点名写入 `.e2e/env`。

最后会打印 `.e2e/env` 的内容，后续步骤和测试都从这里读取环境信息：

```console
$ make e2e-cluster
...
cilium chart: cilium-1.20.5
daemon set "cilium" successfully rolled out
...
test/scripts/setup-testenv.sh
node/egw-e2e-control-plane labeled
node/egw-e2e-worker labeled
node/egw-e2e-worker2 labeled
pod/client created
pod/client condition met
wrote /root/cyclinder/cilium-egressgateway-ha/.e2e/env:
KUBECONFIG=/root/cyclinder/cilium-egressgateway-ha/.e2e/kubeconfig
E2E_KIND_CLUSTER=egw-e2e
E2E_CONTROL_PLANE=egw-e2e-control-plane
E2E_CONTROL_PLANE_IP=172.21.0.2
E2E_WORKER1=egw-e2e-worker
E2E_WORKER2=egw-e2e-worker2
E2E_CLIENT_NODE=egw-e2e-worker3
E2E_TARGET_ADDR=172.21.255.250:8080
E2E_EGRESS_IP_RANGE=172.21.255.195-172.21.255.205
E2E_NAMESPACE=kube-system
```

`cilium chart` 一行是实际安装的 Cilium 版本。IP 地址取决于 docker 给 kind 网络分配的网段，每台机器可能不同。

这一步可以重复执行，已经存在的集群和 Cilium 不会重建。

### 3.2 构建镜像并安装

```bash
make e2e-deploy
```

从当前代码编译 controller 和 agent，构建镜像 `ghcr.io/spidernet-io/cilium-egressgateway-ha/controller:dev` 和 `.../agent:dev`，用 `kind load` 加载到集群中，不推送到镜像仓库。然后 apply CRD，用仓库中的 chart 安装，并重启组件让新镜像生效。

```console
$ make e2e-deploy
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/linux_amd64/ ./cmd/...
for c in controller agent; do \
	docker build --build-arg TARGETARCH=amd64 --build-arg COMPONENT=$c \
		-t ghcr.io/spidernet-io/cilium-egressgateway-ha/$c:dev . || exit 1; \
done
...
#7 naming to ghcr.io/spidernet-io/cilium-egressgateway-ha/controller:dev done
...
#7 naming to ghcr.io/spidernet-io/cilium-egressgateway-ha/agent:dev done
...
Image: "ghcr.io/spidernet-io/cilium-egressgateway-ha/agent:dev" with ID "sha256:2b0c797e..." not yet present on node "egw-e2e-worker", loading...
...
Release "egwha" does not exist. Installing it now.
STATUS: deployed
...
daemon set "egwha-agent" successfully rolled out
deployment "egwha-controller" successfully rolled out
```

修改代码后，重新执行这一步即可，不需要重建集群。

### 3.3 运行测试

```bash
make e2e-test
```

按顺序运行 `test/e2e` 下的所有 suite，测试报告写到 `.e2e/report/junit.xml`。全部通过时，每个 suite 的结尾和整体的结尾如下：

```console
$ make e2e-test
...
Running Suite: CRD Suite - .../test/e2e/crd
SUCCESS! -- 2 Passed | 0 Failed | 0 Pending | 0 Skipped
Running Suite: Failover Suite - .../test/e2e/failover
SUCCESS! -- 2 Passed | 0 Failed | 0 Pending | 0 Skipped
Running Suite: Reliability Suite - .../test/e2e/reliability
SUCCESS! -- 6 Passed | 0 Failed | 0 Pending | 0 Skipped
Running Suite: Switchover Suite - .../test/e2e/switchover
SUCCESS! -- 6 Passed | 0 Failed | 0 Pending | 0 Skipped
Running Suite: Webhook Suite - .../test/e2e/webhook
SUCCESS! -- 9 Passed | 0 Failed | 0 Pending | 0 Skipped
Ginkgo ran 5 suites in 7m9.960106501s
Test Suite Passed
```

故障场景的用例会把实测数据记录在报告中，例如：

```console
  crash -> switched - .../test/e2e/failover/failover_test.go:61
    16.9s
  crash -> egress traffic recovered - .../test/e2e/failover/failover_test.go:62
    19.6s
```

| suite | 内容 | label |
|---|---|---|
| `crd` | CRD 校验 | |
| `webhook` | policy 和 `EgressGatewayHA` 的 webhook | |
| `switchover` | 选主、agent 重启、手动切换、重复地址冲突、移出出口节点 | |
| `failover` | kubelet 停止导致 NotReady、节点宕机靠 TCP 探测发现 | `disruptive` |
| `reliability` | kubelet、agent、cilium-agent、controller 重启，API Server 不可用，网络分区 | `disruptive` |

`disruptive` 的用例会 kill 并重启节点容器、停止 kubelet、停止 API Server 约 1 分钟、用 iptables 隔离节点。跳过它们：

```console
$ make e2e-test E2E_LABEL='!disruptive'
...
Running Suite: CRD Suite - .../test/e2e/crd
SUCCESS! -- 2 Passed | 0 Failed | 0 Pending | 0 Skipped
Running Suite: Failover Suite - .../test/e2e/failover
SUCCESS! -- 0 Passed | 0 Failed | 0 Pending | 2 Skipped
Running Suite: Reliability Suite - .../test/e2e/reliability
SUCCESS! -- 0 Passed | 0 Failed | 0 Pending | 6 Skipped
Running Suite: Switchover Suite - .../test/e2e/switchover
SUCCESS! -- 6 Passed | 0 Failed | 0 Pending | 0 Skipped
Running Suite: Webhook Suite - .../test/e2e/webhook
SUCCESS! -- 9 Passed | 0 Failed | 0 Pending | 0 Skipped
Ginkgo ran 5 suites in 47.472564458s
Test Suite Passed
```

## 4. 只运行部分用例

先导入环境变量，再直接用 ginkgo 运行：

```bash
set -a; . .e2e/env; set +a

# 只运行一个 suite
go tool ginkgo --tags e2e -v test/e2e/webhook
```

只运行名字匹配的用例：

```console
$ go tool ginkgo --tags e2e --focus 'DAD conflict' test/e2e/switchover
...
Will run 1 of 6 specs
SSSS•S

Ran 1 of 6 Specs in 16.277 seconds
SUCCESS! -- 1 Passed | 0 Failed | 0 Pending | 5 Skipped
PASS

Ginkgo ran 1 suite in 19.730007095s
Test Suite Passed
```

导入环境变量后，`kubectl` 也会使用 e2e 集群，方便排查问题：

```console
$ kubectl get egressgatewayha
NAME      ACTIVE            REACHABLE   READY   AGE
default   egw-e2e-worker2   True        True    3m41s

$ kubectl -n kube-system get pods -l app.kubernetes.io/instance=egwha
NAME                               READY   STATUS    RESTARTS   AGE
egwha-agent-46tcz                  1/1     Running   0          3m58s
egwha-agent-g7m4g                  1/1     Running   0          3m30s
egwha-agent-vtkvc                  1/1     Running   0          2m59s
egwha-controller-86cc4b87f-6xtbz   1/1     Running   0          3m59s
egwha-controller-86cc4b87f-w97n7   1/1     Running   0          4m
```

## 5. 测试失败时

收集日志：

```console
$ make e2e-logs
OUTPUT_DIR=/root/cyclinder/cilium-egressgateway-ha/.e2e/logs test/scripts/collect-logs.sh
logs collected in /root/cyclinder/cilium-egressgateway-ha/.e2e/logs

$ ls .e2e/logs
ciliumnodes.yaml
egressgatewayha.yaml
events.txt
kind-logs
logs-cilium-6pw92.txt
logs-cilium-c8lcj.txt
logs-cilium-glcbg.txt
logs-cilium-wkkw7.txt
logs-egwha-agent-46tcz.txt
logs-egwha-agent-g7m4g.txt
logs-egwha-agent-vtkvc.txt
logs-egwha-controller-86cc4b87f-6xtbz.txt
logs-egwha-controller-86cc4b87f-w97n7.txt
node-egw-e2e-control-plane.txt
node-egw-e2e-worker2.txt
node-egw-e2e-worker3.txt
node-egw-e2e-worker.txt
nodes.txt
pods.txt
policies.yaml
```

日志保存在 `.e2e/logs` 中，包括：

- `EgressGatewayHA`、policy、Node、CiliumNode 的 YAML
- 所有 Pod 的状态和 Event
- controller、agent、cilium-agent 的日志
- 每个节点的 IP 地址、邻居表和 iptables 规则
- `kind export logs` 导出的节点日志

GitHub Actions 中测试失败时，会自动收集这些日志，和测试报告一起作为 artifact `e2e-report` 上传。

## 6. 清理

```bash
make e2e-clean
```

删除 kind 集群、外部目标容器、docker 网络 `egw-e2e`，以及 `.e2e` 目录。

## 7. 可以修改的变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `E2E_KIND_CLUSTER` | `egw-e2e` | kind 集群名，也是 docker 网络名 |
| `CILIUM_VERSION` | 空，使用最新版本 | Cilium chart 版本 |
| `KIND_NODE_IMAGE` | kind 默认镜像 | 指定 Kubernetes 版本，例如 `kindest/node:v1.34.0` |
| `E2E_LABEL` | 空 | ginkgo 的 label 过滤 |
| `E2E_TIMEOUT` | `90m` | 测试的总超时时间 |
| `TAG` | `dev` | 构建的镜像 tag |

## 8. 编写新用例

- 每类功能一个目录，放在 `test/e2e/<类别>/` 下，包含 `<类别>_suite_test.go` 和用例文件，文件开头加 build tag `//go:build e2e`。
- 公共方法放在 `test/e2e/common`，例如等待切换完成的 `WaitSteady`、检查出口流量的 `WaitSourceIP` 和 `StartMonitor`。
- 节点名、目标地址、出口 IP 段通过 `common.E` 读取，不要写死。
- 会中断控制面或节点的用例，打上 `Label("disruptive")`。
