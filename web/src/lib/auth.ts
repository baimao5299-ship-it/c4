// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 统一登录态：唯一槽位 userAuth，管理端与用户端共用。
// 角色随 token 同槽存储：platform_admin 凭同一 JWT 可访问 /admin 与 /user 两端（后端 middleware 已支持）。
const TOKEN_KEY = 'c3api_user_token'
const ROLE_KEY = 'c3api_user_role'
export const userAuth = {
  getToken: () => localStorage.getItem(TOKEN_KEY),
  setToken: (t: string) => localStorage.setItem(TOKEN_KEY, t),
  getRole: () => localStorage.getItem(ROLE_KEY),
  setRole: (r: string) => localStorage.setItem(ROLE_KEY, r),
  clear: () => {
    localStorage.removeItem(TOKEN_KEY)
    localStorage.removeItem(ROLE_KEY)
  },
}
