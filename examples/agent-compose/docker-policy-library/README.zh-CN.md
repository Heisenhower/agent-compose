# agent-compose 项目级策略库示例

语言：[English](README.md) | 中文

本示例演示项目级 `policies` 策略库：按用途定义策略块，Agent 通过名称选择，另有一个 Agent 直接内联声明策略。运行时使用 Docker 驱动。

`config`、`up` 和 `ls` 只涉及控制面，不需要模型或 API key。

## 前置条件

- `agent-compose` daemon 正在运行。
- 只有真正启动 Agent sandbox 时才需要 Docker。
- 真正运行前需要本地已有 `agent-compose-guest:latest`。

## 配置文件

```yaml
name: docker-policy-library

policies:
  locked-down:
    network:
      default: deny
      allow:
        - host: api.github.com
          port: 443
          protocol: https
  keep-runtime:
    stopped_runtime_policy: retain

agents:
  reviewer:
    provider: codex
    image: agent-compose-guest:latest
    policy: locked-down
    driver:
      docker: {}

  builder:
    provider: codex
    image: agent-compose-guest:latest
    policy: keep-runtime
    driver:
      docker: {}

  adhoc:
    provider: codex
    image: agent-compose-guest:latest
    policy:
      network:
        default: allow-all
    driver:
      docker: {}
```

## 引用规则

- `policy: <名称>` 引用 `policies` 中的条目。名称在项目规范化时解析，每个 Agent 最终携带自己的有效策略块。
- 把策略块原样内联，会得到完全相同的规范化和 spec hash，因此把共享块搬进 `policies` 不改变引擎实际执行的内容。
- 引用不存在的名称会报错，绝不会被当成"没有策略"——未声明的策略意味着出网不受限制。
- 即使没有 Agent 引用，每个条目也会被校验；条目本身不能再引用其他策略。
- 策略库只是编写便利，不属于规范化配置：`config` 展示的是每个 Agent 的有效 `policy` 块。

## 校验与应用

在示例目录下：

```bash
agent-compose config
agent-compose up
agent-compose ls
```

在仓库根目录下，无需安装二进制：

```bash
go run ./cmd/agent-compose --file examples/agent-compose/docker-policy-library/agent-compose.yml config
go run ./cmd/agent-compose --file examples/agent-compose/docker-policy-library/agent-compose.yml up
```

规范化输出中 `policies` 已被解析掉，每个 Agent 直接携带它选中的块（完整输出见英文文档）。

## Docker 驱动与 `default: deny`

`reviewer` 声明了 `default: deny`。Docker 驱动无法在保持引擎自身 LLM facade 和 telemetry 端点可达的同时表达这些 allow 规则，因此引擎会**拒绝创建**这个 sandbox，并给出带驱动名的错误，而不是静默地以不受限出网运行。`config` 和 `up` 不会创建 sandbox，所以可以成功；真正 `run` `reviewer` 时会 fail closed。

如需真正运行，请使用 `builder` 或 `adhoc`，或把 `locked-down` 改成 `default: allow-all`（声明仍会被校验、哈希和保存，只是不会拒绝任何流量）。

## 清理

```bash
agent-compose down
```
