// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

// 内置设置注册表（类型化配置）：key/type/value 默认值。管理面 PUT 落库覆盖；
// DB 无行即默认（Get 读路径免初始化）。新增内置项 = 在此追加 + 管理面允许列表
// 同步（service.ValidateSetting 用）。
var DefaultSettings = []Setting{
	{Key: "signup_enabled", Type: SettingTypeSwitch, Value: "true"},
	// 新用户初始资源：公开注册路径应用；管理面 CreateUser 不套默认（显式传值）。
	{Key: "default_user_max_concurrency", Type: SettingTypeNumber, Value: "0"}, // 0 = 不限
	{Key: "default_user_balance", Type: SettingTypeNumber, Value: "0"},         // 最小单位
	{Key: "default_user_temp_balance", Type: SettingTypeNumber, Value: "0"},    // 0 = 不送
	{Key: "default_user_temp_balance_ttl_days", Type: SettingTypeNumber, Value: "30"},
	// litellm 模型价格同步（Phase 5 计费价格来源）：worker 定期拉取 + 管理端手动设价。
	{Key: "price_source_url", Type: SettingTypeString,
		Value: "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"},
	{Key: "price_sync_cron", Type: SettingTypeString, Value: "0 3 * * *"}, // cron 表达式（gronx 解析）
	// service_tier 转发策略（Phase 5 计费）：priority/flex/fast 请求分别按对应 key
	// 处理转发体——passthrough（默认，原样转发）/ strip（删除该字段）/ reject
	// （400 拒绝，不转发）；auto/空恒透传。值域校验见 service.UpdateSetting。
	{Key: "service_tier_policy_priority", Type: SettingTypeString, Value: "passthrough"},
	{Key: "service_tier_policy_flex", Type: SettingTypeString, Value: "passthrough"},
	{Key: "service_tier_policy_fast", Type: SettingTypeString, Value: "passthrough"},
	// 集群实例数 N（#14 多实例预算分摊，设计文档 §3.1）：存 DB settings——
	// 所有实例必须读到同一 N（config 文件各实例可漂移，DB 是唯一共识源）；
	// N 变更走 settings NOTIFY 天然传播。service.ClusterInstances 读取，
	// 缺失/非法回退 1（单实例语义）。
	{Key: "cluster.instances", Type: SettingTypeNumber, Value: "1"},
}

// DefaultSetting 返回内置 key 的默认设置；未知 key 返回 nil。
func DefaultSetting(key string) *Setting {
	for i := range DefaultSettings {
		if DefaultSettings[i].Key == key {
			s := DefaultSettings[i]
			return &s
		}
	}
	return nil
}
