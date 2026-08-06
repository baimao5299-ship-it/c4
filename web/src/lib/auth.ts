const KEY = 'gpm_admin_token'
export const auth = {
  getToken: () => localStorage.getItem(KEY),
  setToken: (t: string) => localStorage.setItem(KEY, t),
  clear: () => localStorage.removeItem(KEY),
}
