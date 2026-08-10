import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createBrowserRouter, Navigate } from 'react-router-dom'
import type { ReactNode } from 'react'
import { ApiClient, ApiUnauthorized } from '@/lib/api/client'
import { ThemeProvider } from '@/components/theme-provider'
import { auth } from '@/lib/auth'
import { Toaster } from '@/components/ui/toast'
import Home from '@/pages/home'
import Login from '@/pages/login'
import Layout from '@/components/layout'
import UserLogin from '@/pages/user/login'
import UserRegister from '@/pages/user/register'
import UserOverview from '@/pages/user/overview'
import UserKeys from '@/pages/user/keys'
import UserLogs from '@/pages/user/logs'
import UserStats from '@/pages/user/stats'
import UserRedemptions from '@/pages/user/redemptions'
import UserLayout from '@/components/user-layout'
import Dashboard from '@/pages/dashboard'
import Templates from '@/pages/templates'
import Accounts from '@/pages/accounts'
import Users from '@/pages/users'
import Groups from '@/pages/groups'
import Logs from '@/pages/logs'
import Stats from '@/pages/stats'
import Rules from '@/pages/rules'
import RedemptionCodes from '@/pages/redemption-codes'
import PricingPage from '@/pages/pricing'
import SettingsPage from '@/pages/settings'

export const api = new ApiClient(auth.getToken)

const router = createBrowserRouter([
  { path: '/', element: <Home /> },
  { path: '/login', element: <Login /> },
  { path: '/user/login', element: <UserLogin /> },
  { path: '/user/register', element: <UserRegister /> },
  {
    path: '/user',
    element: <UserLayout />,
    children: [
      { index: true, element: <UserOverview /> },
      { path: 'keys', element: <UserKeys /> },
      { path: 'logs', element: <UserLogs /> },
      { path: 'stats', element: <UserStats /> },
      { path: 'redemptions', element: <UserRedemptions /> },
    ],
  },
  {
    path: '/app',
    element: <RequireAdmin><Layout /></RequireAdmin>,
    children: [
      { index: true, element: <Navigate to="/app/dashboard" replace /> },
      { path: 'dashboard', element: <Dashboard /> },
      { path: 'templates', element: <Templates /> },
      { path: 'accounts', element: <Accounts /> },
      { path: 'users', element: <Users /> },
      { path: 'groups', element: <Groups /> },
      { path: 'logs', element: <Logs /> },
      { path: 'stats', element: <Stats /> },
      { path: 'rules', element: <Rules /> },
      { path: 'redemption-codes', element: <RedemptionCodes /> },
      { path: 'pricing', element: <PricingPage /> },
      { path: 'settings', element: <SettingsPage /> },
    ],
  },
])

// 管理端路由守卫：本地 role 必须为 platform_admin 才放行 /app 子树。
// 安全默认：token 存在但 role 缺失（旧会话残留、localStorage 被手动清理）一律视为无权限。
// 后端鉴权仍在（非 platform_admin JWT → 401 → handleAuthError 清 token 跳 /login），此守卫只是前端第一层拦截。
function RequireAdmin({ children }: { children: ReactNode }) {
  if (auth.getRole() !== 'platform_admin') return <Navigate to="/login" replace />
  return <>{children}</>
}

// 401 全局拦截（Task 2→3 handoff 硬性要求）：任何 query/mutation 收到
// ApiUnauthorized（client.ts 对 401 响应的归一化）→ 清 token + 跳 /login。
// 页面无需各自 onError 兜底；QueryCache/MutationCache 的 onError 在 React Query
// v5 中对所有活跃观测者/变更统一触发（queries.retry: 0 保证每个请求只报一次）。
const handleAuthError = (err: unknown) => {
  if (err instanceof ApiUnauthorized) {
    auth.clear()
    router.navigate('/login')
  }
}

const qc = new QueryClient({
  queryCache: new QueryCache({ onError: handleAuthError }),
  mutationCache: new MutationCache({ onError: handleAuthError }),
  defaultOptions: {
    queries: { retry: 0, refetchOnWindowFocus: false },
    mutations: { retry: 0 },
  },
})

export default function App() {
  return (
    <ThemeProvider defaultTheme="system" storageKey="vite-ui-theme">
      <QueryClientProvider client={qc}>
        <RouterProvider router={router} />
      </QueryClientProvider>
      <Toaster />
    </ThemeProvider>
  )
}
