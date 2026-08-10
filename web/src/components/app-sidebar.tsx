import { NavLink, useNavigate } from 'react-router-dom'
import { LogOut, type LucideIcon } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { userAuth } from '@/lib/auth'
import { Button } from '@/components/ui/button'

interface NavItem {
  to: string
  key: string
  icon: LucideIcon
  end?: boolean
}

interface NavGroup {
  titleKey?: string
  items: NavItem[]
}

// 管理端（/app）与用户端（/user）共享的侧边栏：品牌 + 分组标题 + 菜单项 + 底部退出。
// 样式在两布局中完全一致（w-56 / bg-sidebar / bg-sidebar-accent 激活态 / 图标 hover 放大）。
export default function AppSidebar({ navs }: { navs: NavGroup[] }) {
  const navTo = useNavigate()
  const { t } = useTranslation()
  return (
    <aside className="w-56 border-r border-sidebar-border bg-sidebar text-sidebar-foreground flex flex-col">
      <div className="p-4 font-semibold text-lg">{t('common.appTitle')}</div>
      <nav className="flex-1 space-y-1 overflow-y-auto p-2">
        {navs.map(group => (
          <div key={group.titleKey ?? group.items[0]?.to}>
            {group.titleKey && (
              <p className="px-3 pt-3 pb-1 text-xs font-medium text-sidebar-foreground/40">{t(group.titleKey)}</p>
            )}
            {group.items.map(({ to, key, icon: Icon, end }) => (
              <NavLink key={to} to={to} end={end}
                className={({ isActive }) =>
                  `group flex items-center gap-2 rounded-md px-3 py-2 text-sm transition-colors ${isActive ? 'bg-sidebar-accent text-sidebar-accent-foreground' : 'text-sidebar-foreground/60 hover:bg-sidebar-accent hover:text-sidebar-accent-foreground'}`}>
                <Icon className="h-4 w-4 transition-transform duration-150 group-hover:scale-110" /> {t(key)}
              </NavLink>
            ))}
          </div>
        ))}
      </nav>
      <div className="p-3 border-t border-sidebar-border">
        <Button variant="ghost" className="w-full justify-start text-sidebar-foreground/60" onClick={() => { userAuth.clear(); navTo('/user/login') }}>
          <LogOut className="h-4 w-4 mr-2" /> {t('common.logout')}
        </Button>
      </div>
    </aside>
  )
}
