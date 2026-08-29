// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 统一登录态：唯一槽位 userAuth，管理端与用户端共用。
// 角色随 token 同槽存储：platform_admin 凭同一 JWT 可访问 /admin 与 /user 两端（后端 middleware 已支持）。
const TOKEN_KEY = 'c3api_user_token'
const ROLE_KEY = 'c3api_user_role'
const MODE_KEY = 'c3api_auth_mode'

// A static admin token is intentionally kept separate from a user JWT at the
// UI layer. The backend accepts both on /api/admin, but only JWTs can access
// /api/user; remembering the mode prevents AppShell from probing /user/me with
// a static token and logging the administrator back out.
export type AuthMode = 'user' | 'admin_token'
export const userAuth = {
  getToken: () => localStorage.getItem(TOKEN_KEY),
  setToken: (t: string) => localStorage.setItem(TOKEN_KEY, t),
  getRole: () => localStorage.getItem(ROLE_KEY),
  setRole: (r: string) => localStorage.setItem(ROLE_KEY, r),
  getMode: (): AuthMode | null => {
    const mode = localStorage.getItem(MODE_KEY)
    return mode === 'user' || mode === 'admin_token' ? mode : null
  },
  setMode: (mode: AuthMode) => localStorage.setItem(MODE_KEY, mode),
  clear: () => {
    localStorage.removeItem(TOKEN_KEY)
    localStorage.removeItem(ROLE_KEY)
    localStorage.removeItem(MODE_KEY)
  },
}
