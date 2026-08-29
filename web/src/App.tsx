// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MotionConfig } from 'framer-motion'
import { Outlet, RouterProvider, createBrowserRouter, Navigate } from 'react-router-dom'
import { lazy, Suspense, type ComponentType, type LazyExoticComponent } from 'react'
import { ApiClient, ApiUnauthorized } from '@/lib/api/client'
import { ThemeProvider } from '@/components/theme-provider'
import { userAuth } from '@/lib/auth'
import { Toaster } from '@/components/ui/toast'
import { FluidCanvas } from '@/components/fluid-canvas'

const AppShell = lazy(() => import('@/components/app-shell'))
const Home = lazy(() => import('@/pages/home'))
const UserLogin = lazy(() => import('@/pages/user/login'))
const UserRegister = lazy(() => import('@/pages/user/register'))
const ForgotPassword = lazy(() => import('@/pages/user/forgot-password'))
const UserOverview = lazy(() => import('@/pages/user/overview'))
const UserModels = lazy(() => import('@/pages/user/models'))
const UserKeys = lazy(() => import('@/pages/user/keys'))
const UserLogs = lazy(() => import('@/pages/user/logs'))
const UserStats = lazy(() => import('@/pages/user/stats'))
const UserRedemptions = lazy(() => import('@/pages/user/redemptions'))
const UserProfile = lazy(() => import('@/pages/user/profile'))
const Forbidden = lazy(() => import('@/pages/forbidden'))
const NotFound = lazy(() => import('@/pages/not-found'))
const Dashboard = lazy(() => import('@/pages/dashboard'))
const Templates = lazy(() => import('@/pages/templates'))
const Upstreams = lazy(() => import('@/pages/upstreams'))
const Accounts = lazy(() => import('@/pages/accounts'))
const Users = lazy(() => import('@/pages/users'))
const Groups = lazy(() => import('@/pages/groups'))
const Logs = lazy(() => import('@/pages/logs'))
const Stats = lazy(() => import('@/pages/stats'))
const Rules = lazy(() => import('@/pages/rules'))
const RedemptionCodes = lazy(() => import('@/pages/redemption-codes'))
const PricingPage = lazy(() => import('@/pages/pricing'))
const SettingsPage = lazy(() => import('@/pages/settings'))
const Ops = lazy(() => import('@/pages/ops'))

function RouteFallback() {
  return (
    <div className="flex min-h-40 items-center justify-center" role="status" aria-label="Loading">
      <span className="h-6 w-6 animate-spin rounded-full border-2 border-current border-r-transparent" />
    </div>
  )
}

function routeElement(Component: LazyExoticComponent<ComponentType>) {
  return <Suspense fallback={<RouteFallback />}><Component /></Suspense>
}

// 唯一登录态 userAuth：管理端 api 与用户端 userApi 同源取 token，
// platform_admin 的 JWT 同样通过 /admin 后端鉴权（middleware 已支持）。
export const api = new ApiClient(userAuth.getToken)

const router = createBrowserRouter([
  { path: '/', element: routeElement(Home) },
  { path: '/user/login', element: routeElement(UserLogin) },
  { path: '/user/register', element: routeElement(UserRegister) },
  { path: '/user/forgot-password', element: routeElement(ForgotPassword) },
  // /app 与 /user 共用单一 AppShell：路由切换只换 Outlet，侧边栏/顶栏不重挂
  {
    path: '/',
    element: routeElement(AppShell),
    children: [
      {
        path: 'app',
        element: <RequireAdmin />,
        children: [
          { index: true, element: <Navigate to="/app/dashboard" replace /> },
          { path: 'dashboard', element: routeElement(Dashboard) },
          { path: 'templates', element: routeElement(Templates) },
          { path: 'upstreams', element: routeElement(Upstreams) },
          { path: 'accounts', element: routeElement(Accounts) },
          { path: 'users', element: routeElement(Users) },
          { path: 'groups', element: routeElement(Groups) },
          { path: 'logs', element: routeElement(Logs) },
          { path: 'stats', element: routeElement(Stats) },
          { path: 'rules', element: routeElement(Rules) },
          { path: 'redemption-codes', element: routeElement(RedemptionCodes) },
          { path: 'pricing', element: routeElement(PricingPage) },
          { path: 'settings', element: routeElement(SettingsPage) },
          { path: 'ops', element: routeElement(Ops) },
        ],
      },
      {
        path: 'user',
        element: <RequireUser />,
        children: [
          { index: true, element: routeElement(UserOverview) },
          { path: 'models', element: routeElement(UserModels) },
          { path: 'profile', element: routeElement(UserProfile) },
          { path: 'keys', element: routeElement(UserKeys) },
          { path: 'logs', element: routeElement(UserLogs) },
          { path: 'stats', element: routeElement(UserStats) },
          { path: 'redemptions', element: routeElement(UserRedemptions) },
        ],
      },
    ],
  },
  // 无匹配路径兜底：必须置于最后，避免吞掉 /user 与 /app 子路由
  { path: '*', element: routeElement(NotFound) },
])

// 管理端路由守卫：未登录一律跳登录页；已登录但角色不足渲染 403 界面。
// 安全默认：token 存在但 role 缺失（旧会话残留、localStorage 被手动清理）一律视为无权限。
// 后端鉴权仍在（非 platform_admin JWT → 401 → handleAuthError 清 token 跳 /user/login），此守卫只是前端第一层拦截。
function RequireAdmin() {
  if (!userAuth.getToken()) return <Navigate to="/user/login" replace />
  if (userAuth.getRole() !== 'platform_admin') return routeElement(Forbidden)
  return <Outlet />
}

// Static ADMIN_TOKEN sessions authenticate only the management API; they do
// not identify a user and must never reach /api/user endpoints. A normal
// platform_admin JWT keeps access to both surfaces.
function RequireUser() {
  if (!userAuth.getToken()) return <Navigate to="/user/login" replace />
  if (userAuth.getMode() === 'admin_token') return <Navigate to="/app/dashboard" replace />
  return <Outlet />
}

// 401 全局拦截（Task 2→3 handoff 硬性要求）：任何 query/mutation 收到
// ApiUnauthorized（client.ts 对 401 响应的归一化）→ 清 token + 跳 /user/login。
// 页面无需各自 onError 兜底；QueryCache/MutationCache 的 onError 在 React Query
// v5 中对所有活跃观测者/变更统一触发（queries.retry: 0 保证每个请求只报一次）。
const handleAuthError = (err: unknown) => {
  if (err instanceof ApiUnauthorized) {
    userAuth.clear()
    router.navigate('/user/login')
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
      <div data-glass-ambient aria-hidden="true">
        <FluidCanvas />
      </div>
      <QueryClientProvider client={qc}>
        <MotionConfig reducedMotion="user">
          <RouterProvider router={router} />
        </MotionConfig>
      </QueryClientProvider>
      <Toaster />
    </ThemeProvider>
  )
}
