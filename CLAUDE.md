# CLAUDE.md — droidpool

## 本项目是开源项目（MIT）

**这个仓库对公网公开。** 版权方 Guangzhou Daboshi Supply Chain Co., Ltd.。
写任何内容进仓库前先过一遍下面的清单，拿不准就不要写。

### 绝对不能进仓库

- 密钥、token、密码、证书私钥、云厂商凭证（AKID / SecretKey / DSN）
- 客户数据、订单、会员、门店经营数据，哪怕是测试用的真实样本
- 内部系统的登录凭证，包括测试账号的工号 / PIN
- 公司中文名、员工姓名、个人邮箱、手机号
- 未公开的业务规则、定价策略、合作方名称

### 已知例外（用户拍板接受，不要再"顺手脱敏"）

- 内网 IP（`192.168.14.x` / `192.168.180.x`）与内网主机别名出现在 docs 与示例配置里。
  它们是私网地址，公网不可达，且脱敏会让操作手册不可用。
- SSH 用户名 `sa`：只是连接串里的用户名，私钥不在仓库。
- 历史提交里有一个误提交的 3.5 MB 二进制（`scrcpy-probe`），不含密钥，经审计决定不改写历史。

### 代码里的默认值

- **CLI 与服务端的默认值不能指向公司内网**。控制面地址、Edge 地址一律要求用户显式配置
  （环境变量或参数），没配就报错说明怎么配，不要偷偷回退到某个内网 IP。
  文档与示例配置里可以出现内网地址作为例子。
- 测试里的 owner / 主机名用中性占位（`dev@host`、`ci-runner`），不用真人名。

### 提交前自查

```bash
git diff --cached | grep -nE 'token|secret|passw|AKID|BEGIN (RSA|EC|OPENSSH)'
git diff --cached | grep -nE '(有限公司|科技|供应链)'   # 中文公司主体名不得出现
```

## 项目是什么

给多个开发 agent（Claude Code / DeepSeek harness 等）并行验证 Android 应用用的
**独占、干净、可丢弃**的 redroid 设备池，附浏览器「设备墙」供操作人员观测与接管。

- 控制面 `cmd/droidpoold`（Go，SQLite，htmx 设备墙，docker over SSH 管节点）
- agent CLI `cmd/droidpool`（claim / run / release，与 worktree 生命周期绑定）
- 设备侧走 scrcpy 协议取 H.264 画面与注入输入（`internal/scrcpy`，协议按 4.1 实测抓包实现）

设计决策与实测数据全在 `docs/`，改动前先看对应文档，别推翻已被数据否定的方案
（例如 screencap 逐帧、swap 做准入闸、全局 adb 锁）。

## 工作语言

代码标识符英文；注释、文档、commit message 中文。**英文 README 是对外门面**，
改功能时同步更新它；中文文档在 `docs/` 下。

## 测试

- `make test`（`go vet` + `-race`）必须全绿才提交。
- 每个包写完测试后做**变异校验**：故意改坏一处被测代码，确认相关测试变红，再还原。
  先 `go build` 确认变异能编译——编译不过的变异等于没做。
- 测试用 `net.Pipe` / 假 Runner 隔离外部进程，不要在单元测试里真调 adb 或 docker。

## 部署

`make dist && deploy/deploy.sh <ssh 别名>`。生产 token 在目标机的 `/opt/droidpool/env`，
配置文件里只留 `${DROIDPOOL_TOKEN}` 占位。
