# 注册令牌一次性占用与崩溃恢复 实施计划

## 背景与现状研究

注册流程位于 [service.go](file:///f:/swe/098834/project-02/backend/internal/usersignup/service.go)，令牌状态完全存放在 Francis actor 状态存储中（每个令牌一个 actor，actor ID = 令牌值，状态 TTL = 令牌有效期）。

当前 `SignUp` 流程：
1. 在 DB 事务外调用 actor `consume`：原子校验并把 `UsageCount++`；
2. 开 DB 事务：`CreateUserInternal` → 签发 access token → 写审计日志 → `Commit` → 提交后 `ScheduleSync`；
3. 任何错误都 best-effort 调用 actor `release`（**无参数盲目 `UsageCount--`**）。

现存缺陷：
- **占用没有尝试身份**：`release` 盲目递减，可能归还另一个并发注册占用的名额（limit=2 时 A、B 各占一次，A 失败 release 后 C 可再注册 → 超出限额）。
- **占用后崩溃**：consume 成功后进程退出，名额永久被占，令牌"已用完"但没有用户。
- **提交结果不确定**：`tx.Commit()` 返回错误但事务实际可能已提交，代码仍 release → 重试创建第二个账户。
- **请求重放**：HTTP 重试没有关联身份，limit=1 时直接报 limit_reached，无法确认/复用已提交用户。
- actor 不可用、令牌过期/删除、回滚/提交未知 没有可区分的诊断结果。

关键事实（已核实）：
- Francis v0.1.1 提供：状态 TTL、`Dispatch` 持久化任务（支持 `WithIdempotencyKey`、`WithJobDelay`、失败重试 + 死信 + `JobFailed` 回调，重启后继续投递）、actor 轮转锁串行化同一 actor 的所有 turn。参考 [backchannellogout/actor.go](file:///f:/swe/098834/project-02/backend/internal/backchannellogout/actor.go) 的持久任务模式。
- [UserCreateDto.ID](file:///f:/swe/098834/project-02/backend/internal/dto/user_dto.go#L25-L38) 支持预选 UUID，`users` 主键约束可天然保证同一尝试至多一个用户。
- 状态为 JSON 持久化；e2e 种子（[e2etest_service.go](file:///f:/swe/098834/project-02/backend/internal/service/e2etest_service.go#L592-L634)）和线上旧状态只含 `UsageCount` 标量，必须向后兼容。
- 注册状态不经过 Pocket ID 自己的 SQL 迁移（actor 状态表归 Francis），**本改动无需新增 SQL 迁移**，不涉及 `TestDBSchemaParity`。

## 设计方案

### 1. 稳定的注册尝试身份

- `signUpDto` 新增可选 `attemptId`（`omitempty,uuid`）。前端注册页加载时用 `crypto.randomUUID()` 生成一次，同一次提交的所有重试复用它；未携带时服务端在令牌路径上生成 UUID。
- `attemptId` 同时作为新用户主键（`UserCreateDto.ID`），把"占用的名额"与"最终提交的 users 行"精确关联；DB 主键保证同一尝试至多一个用户。

### 2. Actor 状态（向后兼容）

```go
type signupAttemptStatus string // pending | completed

type signupTokenAttempt struct {
    ID        string                // = attemptId = 未来用户 ID
    Status    signupAttemptStatus
    CreatedAt time.Time
}

type SignupTokenState struct {
    ID, UserGroupIDs, CreatedAt, ExpiresAt // 不变
    UsageLimit int
    UsageCount int                        // 保留：权威的已占名额标量（旧状态/种子只有它）
    Attempts   []signupTokenAttempt       // 新代码的逐次尝试台账（可为 nil=旧数据）
}
```

不变量：`UsageCount` = 占用名额（pending + completed 都占位，与现有"consume 即计数"语义一致，管理列表/`0 of 8` 展示不变）。旧状态 `Attempts==nil` 时 N 个名额视为 N 个不可释放的遗留 completed 占用。

### 3. Actor 方法（全部在同一 actor turn 锁内串行）

- `consume`，请求 `{AttemptID}`：
  - 不存在/过期 → `not_found`；`UsageCount >= UsageLimit` → `limit_reached`（维持外部 401）。
  - 台账中已有该 AttemptID（pending 或 completed）→ 返回 `ok` + `Replayed=true`（不重复占位、不重复计数）。
  - 否则追加 pending 尝试、`UsageCount++`、保存状态，并 `Dispatch(WithJobDelay(约 2 分钟), WithIdempotencyKey(AttemptID))` 一个持久对账任务；重放分支也 Dispatch（幂等合并，自愈"状态已写但任务丢失"）。
- `complete` `{AttemptID}`：pending→completed，名额不动；尝试不存在而状态还在（对账已释放但提交晚到）则补记 completed 并 `UsageCount++`（保住已提交用户的名额）并告警；completed 再调为 no-op。**completed 永不被释放。**
- `release` `{AttemptID}`：仅当该 ID 的尝试存在且为 pending 时移除并 `UsageCount--`；completed/不存在 → no-op。因此绝不会递减别的并发注册。
- `reconcile` `{AttemptID, UserExists}`，返回动作 `completed | released | deferred | noop`：
  - pending + 用户已存在 → completed；
  - pending + 用户不存在且尝试年龄 < 宽限期 → `deferred`（不动名额，等待重试）；
  - pending + 用户不存在且超过宽限期 → released（仅移除本尝试并 `UsageCount--`）；
  - 非 pending/无状态 → noop。
- `create`/`migrate`/`delete` 保持原行为。
- 新增 `Job`（方法名 `reconcile-attempt`）：负载 `{AttemptID}`，经注入的 reconciler 查 DB：用户存在→complete 并回调 `AttemptConfirmed`（补排 SCIM）；不存在→reconcile，`deferred` 或 DB 查询出错时返回普通错误让框架重试；永久坏数据返回 `ErrJobPermanentFailure`。`JobFailed` 记录含令牌/尝试 ID 的诊断日志（安全方向：名额保持占用）。
- actor 注册加 `local.WithMaxAttempts(5)`，其余默认。

### 4. Service.SignUp 新流程（令牌路径）

1. 开放注册门槛判断不变。
2. 解析/生成 `attemptId`；调用 actor `consume {AttemptID}`：
   - `ErrNoHost/ErrActorHalted/ErrActorNotHosted/context 超时` 等 → 新增 `apperror.SignupTemporarilyUnavailable()`（HTTP 503，code `signup_temporarily_unavailable`，提示稍后用同一请求重试；不做任何补偿）；其余 invoke 错误 → 500。
   - `not_found`（过期/删除）、`limit_reached` → 维持 `TokenInvalidOrExpired` 401（内部状态仍可区分，日志可诊断）。
3. 按主键查用户 `id=attemptId`：
   - **已存在（重放/恢复）**：best-effort `complete`，为已有用户**重新签发** access token（审计已随首个事务写入，不重复写；SCIM 用幂等的 `ScheduleSync` 补排），返回该用户。不创建账户。
4. 不存在 → 事务内用固定 `attemptId` 创建用户（默认组、custom claims 顺序不变）、签发 token、写审计、提交。
5. 事务后结果处理：
   - 成功确认 → best-effort `complete`，然后 `ScheduleSync`（维持原有"提交后单次调度"顺序）。
   - **任何错误**（含 profile 冲突、提交失败）→ 统一对账（不做盲目 release）：用新连接查 `id=attemptId`：
     - 用户存在（含不确定提交实际成功、并发同 ID 竞争落败）→ `complete` + `ScheduleSync` + 重签 token，按成功返回；
     - 用户不存在 → `release{AttemptID}`（只归还本尝试名额），返回原始错误；
     - 查询本身出错 → 不归还，返回错误，持久对账任务兜底。
   - complete/release 调用失败仅记日志：持久对账任务（及启动扫描）是兜底。
6. 开放注册（无令牌）与 `SignUpInitialAdmin` 路径完全不改（不固定 ID、不调 actor）。

### 5. 重启恢复（启动扫描）

在现有 `RunSignupTokenMigration`（host ready 后执行、阻塞至 ctx 取消）中，迁移之后追加 `recoverPendingAttempts(ctx)`：
- `ListStates` 分页枚举所有令牌状态，对每个 pending 尝试批量查 DB 用户是否存在，再调 actor `reconcile`；
- `deferred`（年轻尝试，另一实例可能正在处理或刚重启）跳过，留给延迟任务；
- 用户存在→completed（补排 SCIM），超过宽限期且无用户→released。
- 覆盖"状态已写、Dispatch 丢失/进程立即被杀"的缺口；与持久任务互为幂等兜底。
- 宽限期（2 分钟）远大于正常请求时长，崩溃场景无存活事务，释放判定安全。
- 令牌过期后状态随 TTL 清除：过期令牌不可再注册，名额无需归还；已提交用户仍在库中（可正常登录/管理），不产生重复。

## 变更文件

- [models.go](file:///f:/swe/098834/project-02/backend/internal/usersignup/models.go)：`signupTokenAttempt`、状态枚举、状态新增 `Attempts`。
- [actor.go](file:///f:/swe/098834/project-02/backend/internal/usersignup/actor.go)：consume/complete/release/reconcile 协议、Job/JobFailed、Dispatch；注入 reconciler。
- [dto.go](file:///f:/swe/098834/project-02/backend/internal/usersignup/dto.go)：`signUpDto.AttemptID`。
- [service.go](file:///f:/swe/098834/project-02/backend/internal/usersignup/service.go)：幂等注册流程、错误后按主键对账、`recoverPendingAttempts`、reconciler 实现（`UserExistsForAttempt`/`AttemptConfirmed`）。
- [module.go](file="file:///f:/swe/098834/project-02/backend/internal/usersignup/module.go")：工厂闭包注入 service（仿 backchannellogout）、`WithMaxAttempts`、启动时运行扫描。
- [error.go](file:///f:/swe/098834/project-02/backend/internal/apperror/error.go) 与 [constructors.go](file:///f:/swe/098834/project-02/backend/internal/apperror/constructors.go)：新增 `CodeSignupTemporarilyUnavailable` + 构造器。
- 测试：更新 `actor_test.go`（release 带 AttemptID 等）、`service_test.go`（现有补偿测试适配），新增：逐尝试释放不串名额、重放幂等（同 ID 至多一用户并返回已有用户）、提交不确定恢复、reconcile 三态与宽限、启动扫描、并发同 ID、503 分类。
- 前端：`user.type.ts` 加 `attemptId?`；[signup/+page.svelte](file:///f:/swe/098834/project-02/frontend/src/routes/signup/+page.svelte) 每页生成一个稳定 UUID 随请求发送（无 attemptId 的旧客户端行为不变）。
- e2e 种子无需改动（`UsageCount` 标量语义保留，fullyUsed 种子仍 limit_reached）。

## 验证

1. `cd backend && go test -tags=exclude_frontend,unit ./internal/usersignup/... ./internal/apperror/...`
2. `go test -tags=exclude_frontend,unit ./...` 全量；`golangci-lint run`。
3. `pnpm check`（前端类型门）、`pnpm lint`、`pnpm format`。
4. 重点人工核对：事务期间零 actor 调用（SQLite 死锁约束保持）；审计只在事务内写一次；SCIM 仅提交确认后调度；初始管理员、开放注册、令牌列表/撤销接口签名不变。
5. 可选：`tests/setup` docker compose + Playwright `user-signup.spec.ts`（需要 Docker，环境允许时）。

## 风险与处理

- **晚提交竞争**（对账释放后原事务才提交）：complete 检测到尝试缺失会补记 completed 并恢复计数（可能触顶限额，记告警日志）；用户已存在是不可撤销事实，绝不重复建人。
- **任务/状态两次存储非原子**：consume 重放分支重复 Dispatch（幂等键去重）+ 启动扫描双兜底。
- **旧状态兼容**：保留 `UsageCount`，新字段为可选切片；迁移测试断言的 `UsageCount` 不受影响。
- **宽限期内活跃请求被误判**：2 分钟延迟 + 框架重试 + 启动扫描跳过年轻尝试；崩溃场景无并发存活事务。
- **503 文案**：由后端消息返回，前端既有错误展示组件直接呈现，不改 i18n 文件。
