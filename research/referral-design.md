# C4 邀请码与返利设计调研

日期：2026-09-04  
范围：邀请码/邀请链接、一次绑定、充值返利、24 小时冻结、兑换码溯源和审计。

## 结论摘要

- **最接近 C4 的成熟实现是 New API**：它已经实现了邀请码（affiliate code）、注册时解析邀请人、邀请链接、邀请人数、待转移奖励和转入主余额；可以复用它的字段与事务边界，但不能直接照搬其“注册即奖励”的策略。
- **New API 没有 24 小时冻结**。C4 需要把奖励从用户余额字段拆成独立的返利账本/待结算项，并由定时任务按 `available_at` 结算。这样不会因为重复回调、重试或管理员补余额而重复发放。
- **Lago 的事件幂等和交易账本值得借鉴**：每个充值事件带稳定的幂等 ID，数据库唯一约束保证重复事件只记一次；返利结算应使用同样的模式。
- 兑换码清理不能物理删除已使用记录。历史兑换码要保留哈希/掩码、创建批次、兑换用户、兑换时间和原始充值金额，才能完成溯源和争议处理。

## 1. New API：邀请码与奖励

项目：<https://github.com/QuantumNous/new-api>（本次核对提交：`3a9f41ee85cc369f5b8d7fe6e62ff4e7bf3a9ec8`）

### 已确认的源码模式

- `model/user.go` 的 `User` 同时保存 `AffCode`、`AffCount`、`AffQuota`、`AffHistoryQuota`、`InviterId`，并为 `AffCode` 建唯一索引。
- `controller/user.go` 注册流程读取请求中的 `aff_code`，通过 `GetUserIdByAffCode` 得到邀请人，再把 `InviterId` 写入新用户；因此邀请码只在注册时绑定，不是每次登录重新计算。
- `web/src/features/auth/sign-up/components/sign-up-form.tsx` 从 URL 的 `?aff=` 保存邀请码，注册请求发送 `aff_code`；`web/src/routes/__root.tsx` 负责保存链接参数。
- `model/user.go` 的 `inviteUser` 使用数据库表达式递增邀请人数和奖励，避免“读旧值后写回”的并发覆盖；`TransferAffQuotaToQuota` 在事务内锁定用户、检查余额、扣减奖励并增加主余额。
- `web/src/features/wallet/components/affiliate-rewards-card.tsx` 展示邀请人数、待转移奖励、历史累计奖励、推荐链接，并提供“转入余额”操作。

### 对 C4 的可迁移点

1. 邀请码字段采用唯一索引；链接参数只作为注册表单的预填值，最终以服务端一次性绑定为准。
2. 邀请关系写入用户表（`inviter_id`）后不可由普通用户修改；禁止自邀、循环邀请和已注册用户补绑。
3. 所有奖励变更使用单条 SQL 表达式或行锁，不使用“查询余额 → 内存相加 → 保存整行”。
4. 用户端显示“待结算”和“可转入”两个状态，管理员端显示邀请关系和奖励来源。

### 与 C4 需求的差异

New API 的 `AffQuota` 是可直接转入的奖励，没有 24 小时冻结，也没有按充值订单记录每笔返利来源。C4 不能只增加一个 `referral_balance` 数字字段，否则无法可靠实现延迟、撤销、审计和重复回调防护。

## 2. Lago：幂等事件与账本思路

项目：<https://github.com/getlago/lago>（API 子仓本次核对提交：`19b80accb4a4fae4b037a8d6aaa2275ef261588a`）  
幂等事件模型：<https://github.com/getlago/lago-api/blob/main/app/models/idempotency_record.rb>  
幂等键唯一索引：<https://github.com/getlago/lago-api/blob/main/db/migrate/20250414091130_create_idempotency_records.rb>  
钱包交易状态：<https://github.com/getlago/lago-api/blob/main/app/models/wallet_transaction.rb>  
API 文档：<https://docs.getlago.com/api-reference/events/usage>

### 可借鉴模式

- 用业务方提供的 `idempotency_id` 标识一次事件，并在数据库建立唯一约束；重复提交返回已有结果，而不是再次入账。
- 事件、计量和结算记录分离，原始事件保留，派生金额可以重算；这比只保存“当前余额”更适合 C4 的返利和兑换码追溯。
- 结算状态应显式建模（例如 `pending`、`available`、`reversed`），而不是用负数或空时间戳表达状态。

### C4 的适配方式

每次有效充值生成稳定的 `topup_event_id`；返利表以 `(inviter_id, topup_event_id)` 唯一约束，确保同一充值无论来自兑换码、管理员加余额还是支付回调，都只产生一笔 5% 返利。记录 `amount`, `rate`, `reward`, `available_at`, `status` 和 `source_type/source_id`，结算任务只处理 `available_at <= now()` 的 `pending` 行。

## 3. 推荐的 C4 数据边界

### 邀请关系

- `users.inviter_id`：注册成功时写入，普通用户不可编辑。
- `users.invite_code`：12 位随机纯英文字母，唯一索引；兑换码与邀请码使用不同命名空间，避免混淆。
- `referral_clicks`（可选）：只记录匿名点击和首次来源，不把点击当成奖励依据。

### 返利账本

建议新增 `referral_rewards`：

| 字段 | 用途 |
|---|---|
| `id` | 主键 |
| `inviter_id` / `invitee_id` | 邀请双方 |
| `source_type` | `redemption` 或 `admin_credit` |
| `source_id` | 兑换记录/管理员操作 ID |
| `base_amount` | 实际增加的用户余额 |
| `rate_bps` | 返利比例，5% 存为 500 基点 |
| `reward_amount` | 计算后的返利金额 |
| `status` | `pending` / `available` / `reversed` |
| `available_at` | 创建后 24 小时 |
| `created_at`, `settled_at` | 审计时间 |
| `idempotency_key` | 唯一约束，阻止重复发放 |

使用整数最小货币单位或固定精度 Decimal；不要用浮点数计算 5%。

### 兑换码溯源

已有兑换码和兑换记录应保留：码批次、创建人、面值、适用分组、首次兑换用户、兑换时间、客户端 IP（按现有隐私策略掩码）和关联返利记录 ID。管理员“清理旧码”只停用未使用码；已使用码及其历史记录不能删除。

## 4. 关键业务规则

1. **一次绑定**：注册请求可带邀请码；服务端验证存在、未过期、不是本人后写入 `inviter_id`。用户创建完成后不接受再次绑定。
2. **返利基数**：仅按实际入账金额计算；不按折扣前金额、支付订单重复回调或用户消费金额计算。兑换码充值和管理员加余额分别记录来源。
3. **24 小时冻结**：返利创建时 `status=pending`、`available_at=created_at+24h`；定时任务以行锁/条件更新把到期行改为 `available`，用户点击提现（转主余额）时再以事务扣待结算、加主余额。
4. **撤销与退款**：充值被撤销时，未结算返利直接改 `reversed`；已结算返利产生反向账务项，不直接覆盖历史行。
5. **幂等**：注册奖励、兑换码兑换、管理员加余额、返利结算和转入主余额都必须有稳定幂等键；并发请求只能有一个成功改变余额。
6. **防刷**：同一设备/IP、短时间大量注册、循环邀请和自邀应进入风控标记；风控标记不能静默改变账本，需保留原因和管理员处理记录。

## 5. 管理台与用户端可见性

### 用户端

- 显示邀请码、可复制邀请链接、已邀请人数、待结算返利、可转入余额和预计解冻时间。
- 邀请码输入只出现在注册页；已登录用户不显示可修改入口。
- 返利明细显示来源类型、来源金额、比例、奖励金额、状态和时间，不暴露其他用户敏感信息。

### 管理端

- 兑换码批次、码状态、兑换用户、兑换时间和关联返利一键追溯。
- 用户详情显示“邀请人”和“被邀请用户”；返利账本支持按用户、来源、状态、时间和幂等键筛选。
- 每条余额/返利日志记录变更前余额、变更额、变更后余额、操作者、来源订单和请求 ID。

## 6. 风险与验收重点

- 不要把 New API 的“即时 AffQuota”直接改名后上线；它无法满足 24 小时冻结和充值来源追踪。
- 不要定时扫描后直接 `UPDATE users SET balance=balance+...`；必须先锁定/条件更新奖励行，再写入一笔可审计的余额流水。
- 不要物理删除兑换历史；否则无法解释返利金额，也无法处理用户争议。
- C4、New API 和 Lago 均采用 AGPL-3.0。当前结论只提炼公开设计；若复制具体代码，仍需保留相应版权和许可证声明。
- 必测并发场景：同一兑换码并发兑换、同一充值回调重复到达、同一返利同时到期结算、转入余额与结算并发、注册重复提交邀请码。

## 参考源码索引

- New API 用户与邀请字段：<https://github.com/QuantumNous/new-api/blob/main/model/user.go>
- New API 注册绑定：<https://github.com/QuantumNous/new-api/blob/main/controller/user.go>
- New API 注册表单/URL 邀请参数：<https://github.com/QuantumNous/new-api/blob/main/web/src/features/auth/sign-up/components/sign-up-form.tsx>
- New API 推荐奖励卡片：<https://github.com/QuantumNous/new-api/blob/main/web/src/features/wallet/components/affiliate-rewards-card.tsx>
- Lago 幂等记录模型：<https://github.com/getlago/lago-api/blob/main/app/models/idempotency_record.rb>
- Lago 钱包交易模型：<https://github.com/getlago/lago-api/blob/main/app/models/wallet_transaction.rb>
