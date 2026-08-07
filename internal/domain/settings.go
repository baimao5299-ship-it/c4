package domain

// 内置设置注册表（类型化配置）：key/type/value 默认值。管理面 PUT 落库覆盖；
// DB 无行即默认（Get 读路径免初始化）。新增内置项 = 在此追加 + 管理面允许列表
// 同步（service.ValidateSetting 用）。
var DefaultSettings = []Setting{
	{Key: "signup_enabled", Type: SettingTypeSwitch, Value: "true"},
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
