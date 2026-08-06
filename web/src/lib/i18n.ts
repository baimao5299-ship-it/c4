import i18n from 'i18next'
import { initReactI18next } from 'react-i18next'
import zh from '@/locales/zh.json'
import en from '@/locales/en.json'

export const LANG_KEY = 'gpm_lang'
export type AppLang = 'zh-CN' | 'en'

// 语言解析顺序：localStorage gpm_lang → navigator.language（zh 开头 → zh-CN，否则 en）→ 默认 zh-CN。
function detectLang(): AppLang {
  const saved = localStorage.getItem(LANG_KEY)
  if (saved === 'zh-CN' || saved === 'en') return saved
  return (navigator.language ?? '').toLowerCase().startsWith('zh') ? 'zh-CN' : 'en'
}

i18n.use(initReactI18next).init({
  resources: {
    'zh-CN': { translation: zh },
    en: { translation: en },
  },
  lng: detectLang(),
  fallbackLng: 'zh-CN',
  interpolation: { escapeValue: false },
  react: { useSuspense: false },
})

// 切换语言并持久化到 localStorage；react-i18next 自动触发全部已挂载组件重渲染。
export function setLang(lng: AppLang) {
  localStorage.setItem(LANG_KEY, lng)
  i18n.changeLanguage(lng)
}

export default i18n
