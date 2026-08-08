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
